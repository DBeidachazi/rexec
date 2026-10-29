package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxCommand  = 1 << 20
	maxFileSize = 100 << 20
	readLimit   = maxFileSize + 4096
)

type request struct {
	Token   string `json:"token"`
	Op      string `json:"op"` // exec, upload, download, shell
	Command string `json:"command,omitempty"`
	Path    string `json:"path,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

type frame struct {
	Type string `json:"type"` // stdout, stderr, exit, error, data, done
	Code int    `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
	Data string `json:"data,omitempty"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "server" {
		server(os.Args[2:])
		return
	}
	client(os.Args[1:])
}

func tokenFromFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read token: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func server(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	tokenFile := fs.String("token-file", "/etc/rexec/token", "token file")
	root := fs.String("root", "/var/lib/rexec/files", "allowed file root")
	unrestricted := fs.Bool("unrestricted", false, "allow any command (no allowlist)")
	maxSeconds := fs.Int("timeout", 60, "command timeout in seconds")
	fs.Parse(args)
	token := tokenFromFile(*tokenFile)
	if len(token) < 24 {
		log.Fatal("token must contain at least 24 characters")
	}
	if err := os.MkdirAll(*root, 0700); err != nil {
		log.Fatal(err)
	}
	h := &handler{token: token, root: *root, timeout: time.Duration(*maxSeconds) * time.Second, unrestricted: *unrestricted}
	mux := http.NewServeMux()
	mux.Handle("/", h)
	s := &http.Server{Addr: *listen, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("rexec server listening on %s", *listen)
	log.Fatal(s.ListenAndServe())
}

type handler struct {
	token, root  string
	unrestricted bool
	timeout      time.Duration
}

var upgrader = websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096, CheckOrigin: func(r *http.Request) bool {
	// CLI clients do not send Origin. Browser requests are rejected by default.
	return r.Header.Get("Origin") == ""
}}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	c.SetReadLimit(readLimit)
	var req request
	if err := c.ReadJSON(&req); err != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.token)) != 1 {
		time.Sleep(2 * time.Second)
		_ = c.WriteJSON(frame{Type: "error", Msg: "authentication failed"})
		return
	}
	log.Printf("authenticated op=%s path=%q", req.Op, req.Path)
	switch req.Op {
	case "exec":
		h.exec(c, req.Command)
	case "upload":
		h.upload(c, req)
	case "download":
		h.download(c, req.Path)
	default:
		_ = c.WriteJSON(frame{Type: "error", Msg: "unsupported operation"})
	}
}

func (h *handler) exec(c *websocket.Conn, command string) {
	if !h.unrestricted && !allowedCommand(command) {
		_ = c.WriteJSON(frame{Type: "error", Msg: "command is not allowlisted"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = c.WriteJSON(frame{Type: "error", Msg: err.Error()})
		return
	}
	errout, err := cmd.StderrPipe()
	if err != nil {
		_ = c.WriteJSON(frame{Type: "error", Msg: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		_ = c.WriteJSON(frame{Type: "error", Msg: err.Error()})
		return
	}
	done := make(chan struct{}, 2)
	stream := func(kind string, r io.Reader) {
		defer func() { done <- struct{}{} }()
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 4096), 4<<20)
		for s.Scan() {
			_ = c.WriteJSON(frame{Type: kind, Data: s.Text() + "\n"})
		}
	}
	go stream("stdout", out)
	go stream("stderr", errout)
	<-done
	<-done
	err = cmd.Wait()
	code := 0
	if err != nil {
		code = 1
		var e *exec.ExitError
		if errors.As(err, &e) {
			code = e.ExitCode()
		}
	}
	_ = c.WriteJSON(frame{Type: "exit", Code: code, Msg: errString(err)})
}

func allowedCommand(s string) bool {
	if len(s) == 0 || len(s) > maxCommand || strings.ContainsAny(s, ";&|<>`$()\n\r") {
		return false
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return false
	}
	allowed := map[string]bool{"uname": true, "uptime": true, "df": true, "free": true, "whoami": true, "id": true, "date": true, "pwd": true, "ls": true, "systemctl": true, "journalctl": true}
	return allowed[fields[0]]
}
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (h *handler) safePath(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be relative")
	}
	clean := filepath.Clean(p)
	if clean == "." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." {
		return "", fmt.Errorf("path escapes root")
	}
	full := filepath.Join(h.root, clean)
	return full, nil
}
func (h *handler) upload(c *websocket.Conn, req request) {
	p, err := h.safePath(req.Path)
	if err != nil {
		_ = c.WriteJSON(frame{Type: "error", Msg: err.Error()})
		return
	}
	if req.Size < 0 || req.Size > maxFileSize {
		_ = c.WriteJSON(frame{Type: "error", Msg: "invalid size"})
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		_ = c.WriteJSON(frame{Type: "error", Msg: err.Error()})
		return
	}
	defer f.Close()
	var got int64
	for got < req.Size {
		_, b, err := c.ReadMessage()
		if err != nil {
			return
		}
		if len(b) > int(req.Size-got) {
			return
		}
		if _, err = f.Write(b); err != nil {
			return
		}
		got += int64(len(b))
	}
	_ = c.WriteJSON(frame{Type: "done"})
}
func (h *handler) download(c *websocket.Conn, path string) {
	p, err := h.safePath(path)
	if err != nil {
		_ = c.WriteJSON(frame{Type: "error", Msg: err.Error()})
		return
	}
	f, err := os.Open(p)
	if err != nil {
		_ = c.WriteJSON(frame{Type: "error", Msg: err.Error()})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() > maxFileSize {
		_ = c.WriteJSON(frame{Type: "error", Msg: "file unavailable or too large"})
		return
	}
	_ = c.WriteJSON(frame{Type: "data", Code: int(st.Size())})
	buf := make([]byte, 32*1024)
	for {
		n, e := f.Read(buf)
		if n > 0 {
			if e := c.WriteMessage(websocket.BinaryMessage, buf[:n]); e != nil {
				return
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return
		}
	}
	_ = c.WriteJSON(frame{Type: "done"})
}

func client(args []string) {
	fs := flag.NewFlagSet("rexec", flag.ExitOnError)
	token := fs.String("p", "", "authentication token")
	serverURL := fs.String("url", "wss://rexec.example.com/", "WebSocket URL")
	upload := fs.String("u", "", "local file to upload")
	download := fs.String("d", "", "remote file to download")
	recursive := fs.Bool("r", false, "reserved: interactive mode is disabled")
	fs.Parse(args)
	_ = recursive
	if *token == "" {
		fmt.Fprintln(os.Stderr, "-p is required")
		fs.Usage()
		os.Exit(2)
	}
	u := *serverURL
	if !strings.HasPrefix(u, "ws://") && !strings.HasPrefix(u, "wss://") {
		u = "wss://" + u
	}
	if *upload != "" {
		if fs.NArg() != 1 {
			log.Fatal("upload usage: rexec -u local remote")
		}
		st, err := os.Stat(*upload)
		if err != nil {
			log.Fatal(err)
		}
		c, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			log.Fatal(err)
		}
		defer c.Close()
		must(c.WriteJSON(request{Token: *token, Op: "upload", Path: fs.Arg(0), Size: st.Size()}))
		f, _ := os.Open(*upload)
		defer f.Close()
		buf := make([]byte, 32*1024)
		for {
			n, e := f.Read(buf)
			if n > 0 {
				must(c.WriteMessage(websocket.BinaryMessage, buf[:n]))
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				log.Fatal(e)
			}
		}
		printFrames(c)
		return
	}
	if *download != "" {
		if fs.NArg() != 1 {
			log.Fatal("download usage: rexec -d remote local")
		}
		c, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			log.Fatal(err)
		}
		defer c.Close()
		must(c.WriteJSON(request{Token: *token, Op: "download", Path: *download}))
		f, err := os.OpenFile(fs.Arg(0), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		for {
			t, b, e := c.ReadMessage()
			if e != nil {
				log.Fatal(e)
			}
			if t == websocket.BinaryMessage {
				if _, e = f.Write(b); e != nil {
					log.Fatal(e)
				}
			} else {
				var fr frame
				if json.Unmarshal(b, &fr) == nil && fr.Type == "done" {
					break
				}
				if json.Unmarshal(b, &fr) == nil && fr.Type == "error" {
					log.Fatal(fr.Msg)
				}
			}
		}
		return
	}
	if fs.NArg() < 2 {
		fs.Usage()
		os.Exit(2)
	}
	host := fs.Arg(0)
	command := strings.Join(fs.Args()[1:], " ")
	if !strings.Contains(host, "://") {
		host = u
	}
	_ = host
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	must(c.WriteJSON(request{Token: *token, Op: "exec", Command: command}))
	printFrames(c)
}
func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
func printFrames(c *websocket.Conn) {
	for {
		t, b, e := c.ReadMessage()
		if e != nil {
			if e != io.EOF {
				log.Print(e)
			}
			return
		}
		if t == websocket.BinaryMessage {
			_, _ = os.Stdout.Write(b)
			continue
		}
		var f frame
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		if f.Type == "stdout" {
			fmt.Print(f.Data)
		} else if f.Type == "stderr" {
			fmt.Fprint(os.Stderr, f.Data)
		} else if f.Type == "error" {
			log.Print(f.Msg)
		} else if f.Type == "exit" {
			os.Exit(f.Code)
		} else if f.Type == "done" {
			return
		}
	}
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

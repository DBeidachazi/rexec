# rexec

简单的 Go 远程命令执行与文件传输工具：客户端通过 WebSocket 连接服务端，命令输出实时回传，支持在指定目录内上传、下载文件。同一二进制包含客户端和服务端。

服务端默认只监听本机，并启用命令白名单；可使用 Caddy 提供 HTTPS/WSS。需要执行任意 shell 命令时，可显式开启 `-unrestricted`，此时权限不受 rexec 限制，完全取决于运行服务端的操作系统用户。

> **安全警告：** `-unrestricted` 允许执行管道、重定向、命令替换等任意 shell 命令，不提供交互式终端或沙箱。如果服务用户拥有 sudo、Docker 等权限，token 泄露可能等同于整台服务器的控制权泄露。公网部署请谨慎评估风险。

## 构建

需要 Go 1.22 或更新版本。

```bash
go mod tidy
go build -buildvcs=false -o rexec .
```

交叉编译静态 Linux 二进制：

```bash
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -o dist/rexec-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -trimpath -o dist/rexec-linux-arm64 .
```

## 服务端安装

以下示例使用独立的低权限 `rexec` 用户和白名单模式：

```bash
sudo install -d -m 0700 /etc/rexec /var/lib/rexec/files
sudo install -o root -g root -m 0755 rexec /usr/local/bin/rexec
sudo openssl rand -hex 32 | sudo tee /etc/rexec/token >/dev/null
sudo chmod 0600 /etc/rexec/token
sudo useradd --system --home /var/lib/rexec --shell /usr/sbin/nologin rexec 2>/dev/null || true
sudo chown -R rexec:rexec /var/lib/rexec
```

`/etc/systemd/system/rexec.service`：

```ini
[Unit]
Description=rexec server
After=network.target

[Service]
User=rexec
Group=rexec
ExecStart=/usr/local/bin/rexec server -listen 127.0.0.1:8080 -token-file /etc/rexec/token -root /var/lib/rexec/files -timeout 60
Restart=on-failure
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/rexec/files
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=multi-user.target
```

启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now rexec
```

### 服务端参数

```text
-listen string      监听地址（默认 127.0.0.1:8080）
-token-file string  token 文件（默认 /etc/rexec/token）
-root string        文件传输根目录（默认 /var/lib/rexec/files）
-timeout int        单条命令超时秒数（默认 60）
-unrestricted       允许任意 shell 命令，不使用白名单（默认关闭）
```

例如，使用当前用户运行不限命令的服务端，并将命令超时设为 900 秒：

```bash
./rexec server \
  -listen 127.0.0.1:8080 \
  -token-file /etc/rexec/token \
  -root "$HOME/rexec-files" \
  -timeout 900 \
  -unrestricted
```

请确保当前用户能读取 token 文件并能访问文件根目录。`root` 参数只限制上传、下载的文件路径，不限制可执行命令访问的文件。

## Caddy

把域名 DNS 指向服务器，在 Caddyfile 中加入：

```caddyfile
rexec.example.com {
    reverse_proxy 127.0.0.1:8080
    request_body {
        max_size 100MB
    }
}
```

Caddy 默认自动申请 HTTPS 证书，客户端使用 `wss://rexec.example.com/`。

## 客户端

```bash
TOKEN='从服务端 /etc/rexec/token 安全复制出来的内容'
URL='wss://rexec.example.com/'

# 流式执行命令；host 是为兼容旧命令行保留的占位参数
./rexec -url "$URL" -p "$TOKEN" host 'uname -a'

# 不限命令模式下可执行管道、重定向等 shell 命令
./rexec -url "$URL" -p "$TOKEN" host 'printf "host: "; hostname; id'

# 上传到服务端文件根目录下的 remote/subdir/file.txt
./rexec -url "$URL" -p "$TOKEN" -u ./local.txt remote/subdir/file.txt

# 从服务端文件根目录下载
./rexec -url "$URL" -p "$TOKEN" -d remote/subdir/file.txt ./local.txt
```

命令中有引号、`$`、分号或管道符时，建议用单引号包住整条远程命令，避免本地 shell 提前展开。

`host` 参数保留了命令行兼容性；实际连接地址由 `-url` 指定。`-r` 当前明确禁用，不提供交互式 shell，因此 `vim`、交互式 `top` 等程序不适用，可使用 `top -bn1` 等一次性输出的方式。

默认白名单包含：`uname`、`uptime`、`df`、`free`、`whoami`、`id`、`date`、`pwd`、`ls`、`systemctl`、`journalctl`，并拒绝 shell 控制符。白名单在 `main.go` 的 `allowedCommand` 中维护。

## 安全说明

- token 至少 24 字符，建议使用 `openssl rand -hex 32` 生成；服务端只校验 token，不区分客户端用户或权限。
- 不要通过聊天、命令历史或不加密文件分发 token；认证失败会延迟 2 秒，但没有按 IP 封禁。
- 建议在防火墙中只开放 443，并限制 Caddy 的来源 IP；不要把 8080 直接暴露到公网。
- 优先使用独立低权限用户和默认白名单。`-unrestricted` 不提供任何命令审计、交互确认或操作系统级隔离。
- 上传下载路径只能是 `-root` 指定目录下的相对路径，单文件最大 100 MiB。
- 命令默认 60 秒超时；单条命令最大 1 MiB，单行流式输出最大 4 MiB。
- 生产环境还应配置日志轮转、备份和监控；临时服务用完后应及时停止。

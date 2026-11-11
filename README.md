# 腾讯微云纯 CLI

`weiyun` 是一个类似 `coscli` 的单进程命令行客户端。它直接调用微云 MCP 接口，不启动网页、HTTP 服务、数据库、Redis 或后台进程。

## 构建

项目固定使用 Go 1.27.3：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o dist/weiyun ./cmd/weiyun
```

生成的 `dist/weiyun` 是 Linux 静态二进制，无 Node、Python、SQLite、Redis 或目标机器 GLIBC 运行时依赖。

## 配置

默认读取：

```text
~/.config/weiyun/config.toml
```

也可显式指定：

```bash
weiyun --config /path/to/config.toml ls wy:/
weiyun ls --config /path/to/config.toml wy:/
```

配置格式见 `configs/config.example.toml`：

```toml
token = "微云 MCP Token"
cookie = "可选 Cookie"
```

建议安装配置：

```bash
mkdir -p ~/.config/weiyun
cp configs/config.example.toml ~/.config/weiyun/config.toml
chmod 600 ~/.config/weiyun/config.toml
```

程序会拒绝组用户或其他用户可读的配置文件。它不会读取项目目录或当前工作目录中的 `token.txt`，也不会在正常输出和上游错误中打印 Token、Cookie 或上游响应正文。

## 命令

`wy:/` 表示当前 Token 的授权根目录，不代表账号中未授权的其他区域。

```bash
weiyun ls wy:/
weiyun ls 'wy:/资料'
weiyun mkdir 'wy:/资料/新目录'

# 上传；目标可以是目录，也可以是明确的目标文件名
weiyun cp ./report.pdf 'wy:/资料/'
weiyun cp ./report.pdf 'wy:/资料/年报.pdf'

# 下载；本地目标可以是目录或尚不存在的文件路径
weiyun cp 'wy:/资料/年报.pdf' ./downloads/
weiyun cp 'wy:/资料/年报.pdf' ./year.pdf

# 同目录改名
weiyun mv 'wy:/资料/旧名.pdf' 'wy:/资料/新名.pdf'

# 跨目录移动时必须保持名称不变
weiyun mv 'wy:/资料/年报.pdf' 'wy:/归档/年报.pdf'

weiyun share 'wy:/资料/年报.pdf'

# 默认进入回收站
weiyun rm 'wy:/资料/年报.pdf'

# 永久删除；-f 可放在路径前后，但不会跳过确认
weiyun rm -f 'wy:/资料/年报.pdf'
weiyun rm 'wy:/资料/年报.pdf' -f
```

路径参数以 `-` 开头时使用标准 `--` 分隔符：

```bash
weiyun cp -- ./-local-name 'wy:/资料/-remote-name'
```

查看帮助不读取配置或凭据：

```bash
weiyun --help
```

## Linux FUSE 挂载

前台挂载当前 Token 授权范围内的目录，挂载点必须是已存在的空目录：

```bash
weiyun mount 'wy:/资料' /mnt/weiyun
weiyun --config /path/to/config.toml mount 'wy:/资料' /mnt/weiyun --cache-dir ~/.cache/weiyun-work
```

挂载支持浏览、目录元数据 TTL 缓存（60s，本地写操作自动失效）、按需分块 Range 读取（1 MiB 稀疏块持久缓存 + 顺序预读）、创建、随机写、截断、`fsync`、mkdir/rmdir、无覆盖 rename 和移入回收站。按 `Ctrl-C` 后停止新写，使用独立超时同步本次会话修改，再正常卸载；同步失败会保留 dirty 缓存并返回非零，不会强制 lazy unmount。

默认缓存位于 `$XDG_CACHE_HOME/weiyun/mounts/<Token指纹>/<远端根指纹>`，无 XDG 时使用 `~/.cache`。缓存按 Token 和远端根隔离，目录权限 `0700`、文件权限 `0600`，并使用排他 `flock`。显式缓存目录若属于其他 Token 或远端根会被拒绝；Token 轮换后的旧 dirty 文件只供手工恢复，不会自动迁移或上传。

打开远程大文件时不会整文件预下载，仅建立本地稀疏文件并按需通过 HTTP Range 拉取访问到的 1 MiB 数据块落盘；已落盘数据块通过本地缓存 fd 到 FUSE 设备的零拷贝路径直接读取，网络 TLS、哈希和 Base64 上传仍保留必要转换。零字节文件不能提交，`fsync`/关闭会报错并保留本地 dirty 数据。

## 删除安全

- `rm` 只请求移入回收站。
- `rm -f` 表示永久删除，不是“强制且跳过确认”。
- 永久删除仅允许在真实交互终端中执行，并要求输入完整的 `DELETE`。
- 非 TTY、EOF、其他回答、授权根 `wy:/` 都会拒绝执行，且不会调用删除接口。
- 不提供 `--yes`、`-y` 或其他确认绕过参数。

## 冲突与传输安全

- 上传、下载、移动和改名均不提供覆盖参数；发现同名目标立即报错。
- 远程路径逐层分页解析；同层重名会报歧义，不会任取第一项。
- `..` 不能越过 `wy:/`；授权根不能删除或移动。
- 跨目录同时改名会被拒绝，只允许同目录改名或跨目录同名移动。
- 下载链接只能来自微云下载接口，且必须是 HTTPS。
- 下载拒绝 localhost、私网、链路本地、多播地址和 DNS 重绑定；跨主机重定向被拒绝，下载 Cookie 不会发送给其他主机。
- 为保持下载目的地址校验，当前下载不使用环境中的 HTTP 代理配置。
- 下载流式写入目标同目录的随机临时文件，校验长度后通过硬链接原子发布；目标已存在时不会被 `rename` 覆盖，失败会清理临时文件。
- 上传哈希和文件数据均流式处理；只为单个受限分片分配内存，不把完整文件读入内存。

## 已知限制

- 参考资料没有证明零字节文件的上传协议，本实现明确返回“不支持”，不会伪造成功。
- `share` 本轮只创建无密码分享，不提供密码、有效期或批量选项。
- 微云接口没有文档化原子 `no-clobber` 能力。本客户端会在写操作前检查同名项，但其他客户端可在检查后并发创建同名目标，因此不能承诺跨客户端绝对无竞态。
- 若上游在创建目录或上传时自行改名，命令会返回失败并报告实际名称；不会把自动改名当作成功。
- FUSE 不支持硬链接、符号链接、xattr、chmod/chown、时间修改、永久删除或覆盖 rename；对应操作返回不支持，跨目录同时改名返回 `EXDEV`。
- 干净缓存每次重新打开前会检查远端 file ID/size/mtime；不一致时不复用旧数据。提交前预检仍不能消除其他客户端在检查后修改文件的竞态。
- dirty/local-only 文件被删除时会移入缓存 `recovery` 目录并保留记录，不直接销毁最后一份本地数据。
- 当前兼容性由 mock/httptest、Python 参考向量和本地 fakeBackend FUSE 测试验证；未使用真实账号执行远端请求、覆盖或永久删除测试。

## 测试与静态检查

```bash
CGO_ENABLED=0 go test -count=1 ./...
WEIYUN_FUSE_TEST=1 CGO_ENABLED=0 go test -count=1 -run TestFUSEIntegration ./internal/mount
CGO_ENABLED=1 go test -race -count=1 ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o dist/weiyun ./cmd/weiyun
file dist/weiyun
ldd dist/weiyun
```

测试覆盖 CLI 参数、永久删除确认、路径分页与重名、顶层 `pdir_key`、无覆盖移动和下载、下载 SSRF/错误清理、MCP JSON-RPC/SSE/错误映射，以及微云特殊 SHA1 固定向量和分片上传状态机。

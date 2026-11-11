# 微云纯 CLI 客户端规格

> 状态：与当前实现一致。原始微云资料保留在 `weiyun/`，不会作为运行时依赖。

## 1. 产品范围

- 产物是单个 `weiyun` CLI 二进制，入口为 `cmd/weiyun/main.go`。
- 固定 Go 1.27.3；Linux 交付使用 `CGO_ENABLED=0` 构建静态二进制。
- 不包含网页、Gin、Vue、HTTP 服务、管理员认证、会话、Redis、SQLite、后台任务或 MCP Server。
- CLI 是微云 MCP 和上传协议客户端，操作范围限定为 Token 授权根 `wy:/`。
- 不实现多账号、同步盘、回收站管理、分享密码、批量分享、覆盖参数或自动 Token 刷新。
- Linux 提供前台可读写 FUSE 挂载；不后台化，用户配置使用 TOML。

## 2. 配置

默认路径：

```text
~/.config/weiyun/config.toml
```

可用全局 `--config <path>` 或 `--config=<path>` 显式指定 TOML 文件。参数可位于子命令前或子命令参数区的 `--` 之前。

TOML 结构：

```toml
token = "required"
cookie = "optional"
```

约束：

- `token` 必填，`cookie` 可选，不允许未知字段。
- 配置权限必须为 `0600` 或更严格。
- 程序不读取 `token.txt`。
- 帮助命令不加载配置。
- Token、Cookie 和上游响应正文不得进入正常输出或错误文本。

## 3. 命令面

```bash
weiyun ls wy:/目录
weiyun mkdir wy:/目录
weiyun cp 本地文件 wy:/目录/文件
weiyun cp wy:/目录/文件 本地路径
weiyun mv wy:/旧路径 wy:/新路径
weiyun share wy:/文件或目录
weiyun rm wy:/文件或目录
weiyun rm -f wy:/文件或目录
weiyun mount wy:/目录 /空目录 [--cache-dir 路径]
```

- `ls`：逐页列出一个远程目录。
- `mkdir`：创建一个目录；创建前检查同名项。
- `cp`：必须恰有一端为远程路径。本地到远程上传，远程到本地下载。
- `mv`：同目录映射 rename；跨目录仅允许保持名称不变并映射 move。
- `share`：创建无密码单对象分享链接。
- `rm`：传递 `delete_completely=false`，移入回收站。
- `rm -f`：传递 `delete_completely=true`，永久删除。
- `mount`：前台挂载远端目录；挂载点必须是已存在空目录，`--cache-dir` 可选并复用全局 `--config`。
- 未知 flag 拒绝；`--` 后的参数按路径字面值处理。

## 4. 永久删除

- `-f` 仅表示永久删除，不表示跳过确认。
- `-f` 可位于 `rm` 路径前或路径后。
- 执行前规范化路径并显示目标。
- stdin 必须通过真实 TTY 检测；非 TTY 直接拒绝。
- 仅输入精确的 `DELETE` 才会继续；其他输入和 EOF 均拒绝。
- 不提供 `--yes` 或其他绕过方式。
- `wy:/` 根目录禁止删除和移动。

## 5. 远程路径与分页

- 远程路径必须以 `wy:/` 开头。
- 重复 `/` 和 `.` 被规范化；`..` 不得越过授权根。
- 每一级目录都通过 `weiyun.list` 分页解析，单页限制为 50。
- 后续页 offset 按实际返回项目数递增。
- 未结束页面返回零项、重复页面或分页期间顶层 `pdir_key` 改变时，返回无进展错误，避免死循环。
- 同层目录和文件统一参与名称匹配；零项为未找到，多项为重名歧义。
- 文件、删除、分享、移动、改名和下载请求使用 `weiyun.list` 响应顶层 `pdir_key`，不使用可能为空的文件项 `pdir_key`。
- context 取消传递到所有 MCP、上传和下载请求。

## 6. 冲突与移动

- 上传、下载、创建目录、移动和改名均在写操作前检查目标，不静默覆盖。
- 上传目标若是已有目录，使用本地文件名；否则将最后一段视为显式目标名。
- 下载目标若是已有目录，使用远程文件名；已有文件或符号链接均报冲突。
- 同目录移动使用 rename。
- 跨目录移动必须保持源名称；跨目录同时改名直接拒绝，不拆成有部分成功风险的多步操作。
- 目录不能移动到自身或后代目录。
- 上游创建或上传返回不同名称时，命令报失败，不把自动改名作为成功。
- 上游没有文档化原子 no-clobber，因此预检查只能避免本客户端的静默覆盖，不能消除其他客户端造成的检查后竞态。

## 7. MCP 客户端

- 默认端点为 `https://www.weiyun.com/api/v3/mcpserver`。
- 每个工具请求注入 `req_header`、`WyHeader: mcp_token=...`，可选发送配置 Cookie。
- 提供 list、download、delete、upload、share、rename、create_dir、move 的 typed 方法。
- 支持普通 JSON 和 SSE 中的 JSON-RPC 响应。
- 区分并脱敏 HTTP、传输、JSON-RPC、工具级和无效响应错误。
- 响应体上限 1 MiB，默认 MCP HTTP 超时 2 分钟。
- MCP 重定向仅允许同 scheme、同 host，避免凭据外泄。
- 写操作不盲目重试；上传分片临时失败后重新预检服务端状态。

## 8. 上传

- 使用 512 KiB 分块和单个共享 SHA1 状态。
- 非最后块记录 h0-h4 小端序内部状态；最后块记录完整文件标准 SHA1。
- 计算 `file_md5`、`file_sha`、`check_sha`、`check_data` 和 `block_sha_list`。
- 算法通过 Python 参考固定向量覆盖 1、63、64、127、128、129、512 KiB 边界和多块文件。
- `io.ReaderAt` 以 64 KiB 哈希缓冲读取，不把完整文件载入内存。
- 分片数据使用一层标准 Base64 JSON 字符串；`upload_key` 和 `ex` 原样传递。
- 分片 offset、长度、文件边界和最大 16 MiB 分片均校验。
- `upload_state=3` 或重复通道持续无进展时终止，避免死循环。
- 零字节上传因资料未证明协议行为而明确返回不支持。

## 9. 下载

- 下载 URL 只接受 HTTPS，拒绝 URL 用户信息。
- 请求前解析并拒绝 localhost、回环、私网、未指定、链路本地和多播 IP。
- 生产 transport 使用已验证 IP 建连，避免校验后再次 DNS 解析造成重绑定。
- 最多跟随 5 次同 host HTTPS 重定向；跨 host 重定向拒绝，Cookie 不外泄。
- HTTP/上游错误不输出响应正文或 Cookie。
- 响应流写入目标同目录随机临时文件，不全量读入内存。
- 完成后校验上游声明的文件长度。
- 使用硬链接发布到最终路径，原子拒绝已存在目标；不使用会覆盖目标的 rename。
- 所有失败路径清理临时文件。

## 10. FUSE 挂载

- 挂载后支持浏览、完整文件缓存读取、创建、随机写、截断、`fsync`、mkdir/rmdir、无覆盖 rename 和移入回收站。
- 默认缓存按 Token 指纹和授权远端根指纹隔离；显式缓存目录的身份不一致时拒绝复用。缓存目录/文件权限分别为 `0700`/`0600`，并持有非阻塞排他 `flock`。
- 缓存 manifest 使用随机 ID，原子写入、文件 `fsync`、rename 和父目录 `fsync`；拒绝符号链接、不安全权限、恶意路径和 cache/mountpoint 包含关系。
- 修改前先持久 dirty；`fsync` 先同步本地数据再上传，远端成功且新 manifest 落盘后才清 dirty。失败、版本冲突、未知提交结果和零字节文件均保留 dirty。
- 崩溃恢复的 dirty 文件不会自动提交；只有本次会话再次修改后，`fsync`/`Flush` 或退出同步才会上传。dirty/local-only 删除会移入 recovery 并保留记录。
- 干净缓存重新打开前比较 file ID、size、mtime；不一致即废弃旧缓存并重新下载。协议没有 CAS，预检不能消除检查后竞态。
- 只读 fd 通过 `ReadResultFd` 返回，并用 `Done` 生命周期保持 fd 和不可变约束；可写句柄读取使用受锁 `pread` 数据返回。读写句柄互斥。
- 不支持硬链接、符号链接、xattr、chmod/chown、时间修改、永久删除、覆盖 rename 和 Range 按需读取；跨目录同时改名返回 `EXDEV`。
- `Ctrl-C` 停止新写，使用独立有界 context 同步本次 dirty，再正常卸载；busy 时不强制 lazy unmount，只有卸载完成才释放 flock。

## 11. 测试与验收

必须通过：

```bash
GO=/root/.cache/tencent-weiyun-go/toolchain/go1.27.3/go/bin/go
CGO_ENABLED=0 "$GO" test -count=1 ./...
WEIYUN_FUSE_TEST=1 CGO_ENABLED=0 "$GO" test -count=1 -run TestFUSEIntegration ./internal/mount
CGO_ENABLED=1 "$GO" test -race -count=1 ./...
"$GO" vet ./...
CGO_ENABLED=0 "$GO" build -trimpath -ldflags='-s -w' -o dist/weiyun ./cmd/weiyun
file dist/weiyun
ldd dist/weiyun
```

mock 测试覆盖：

- CLI 全局参数、`--`、未知 flag、双向 cp 和帮助免凭据；
- `rm`/`rm -f` 映射、TTY、确认、拒绝和 EOF；
- 路径规范化、分页、重名、无进展和顶层 `pdir_key`；
- mkdir/upload/share/move/delete 请求映射与无覆盖；
- 下载 SSRF、防重定向泄露、流式发布、长度和错误清理；
- MCP JSON、SSE、响应限长、超时、取消和错误脱敏；
- 特殊 SHA1 固定向量、Base64、分片边界、临时/永久错误和无进展；
- FUSE cache 锁/权限/身份绑定/恶意 manifest、dirty 恢复、失败保留、版本冲突、重复 Flush、零字节和 fd 读写互斥；
- fakeBackend 的真实本地 FUSE 浏览、fd 读取、创建写回、truncate、失败同步、删除、rename 和卸载。

真实兼容未知：实现和测试不读取真实凭据、不调用真实微云端点、不执行真实上传、覆盖或永久删除。实际账号兼容性仍需由使用者在非破坏性范围内单独验证。

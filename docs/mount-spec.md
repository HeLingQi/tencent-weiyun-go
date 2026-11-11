# Linux 可读写 FUSE 挂载最小规格

> 状态：已审核并实施；保持现有命令、配置 TOML 和非挂载远端行为兼容。

## 1. 目标与边界

- 新命令：`weiyun mount wy:/目录 /挂载点 [--cache-dir 目录]`。
- Linux 前台运行，`Ctrl-C` 有序同步、卸载并退出；不后台化。
- 最小能力：浏览、整文件缓存读取、创建、随机写、截断、`fsync`、关闭、mkdir、rmdir、无覆盖 rename、移入回收站。
- 不支持硬链接、符号链接、xattr、永久删除、覆盖 rename、Range 按需读取；必须返回明确错误，不能假成功。
- 仍使用 Go 1.27.3 和 `CGO_ENABLED=0` 静态构建；锁定 `github.com/hanwen/go-fuse/v2 v2.12.1`。
- 本规格只允许本地/mock 测试；不读取 `token.txt`，不执行真实上游读写。

## 2. 命令与缓存目录

```bash
weiyun [--config 配置] mount wy:/目录 /挂载点 [--cache-dir 目录]
```

- 远端路径沿用 `remote.Normalize`，不能越过授权根；挂载点必须是已存在的空目录。
- `--cache-dir` 可显式指定；省略时使用 `$XDG_CACHE_HOME/weiyun/mounts/<token指纹>/<远端根指纹>`，无 XDG 时使用 `~/.cache`。
- 指纹使用 SHA-256 截断值，只用于隔离授权和远端根；不输出 Token/Cookie，不写入现有配置 TOML。
- Token 轮换会得到新默认目录；旧目录中的脏文件不会自动迁移或上传，可用显式 `--cache-dir` 恢复。
- 缓存目录权限为 `0700`，数据和元数据为 `0600`；拒绝符号链接缓存根。
- 缓存根持有 `.mount.lock` 的非阻塞独占 `flock`；同一目录只能有一个挂载进程。

## 3. 缓存模型

- 每个文件有持久数据文件和原子更新的元数据：远端路径、file_id、parent_key、size、mtime、dirty、local-only、最后错误。
- 元数据写入临时文件，`fsync` 后 rename，并同步父目录；dirty 只在确认远端上传成功后清除。
- 第一次只读打开先下载完整文件到同缓存目录临时文件，校验大小、同步并原子发布，再打开缓存 fd。
- 首次打开延迟等于完整下载；空间至少需要文件大小，下载发布期间可能临时需要约两倍空间。
- 本阶段不默认实现 Range；不能宣称已有按需读取。
- 写、截断和首次脏标记必须持有每文件独占锁；多个写句柄直接返回 `EBUSY`。
- 干净只读句柄使用不可变缓存代，允许 `ReadResultFd`；存在只读句柄时拒绝写打开和 truncate，避免发送期间 fd 内容变化。
- 可写句柄内的读取使用受锁的 `pread` + `ReadResultData`，不冒充 fd 零拷贝。
- 崩溃后保留 dirty 数据；重启不后台上传。恢复文件只有在本次会话再次修改并显式 `fsync`/关闭时才尝试提交。
- 提交前比较已记录的 file_id、size、mtime；不一致返回 `ESTALE` 并保留本地 dirty 数据。

## 4. 写回与错误语义

- 第一次修改前持久化 dirty 标记；普通 write 成功只表示写入本地缓存，不表示远端成功。
- `Fsync`：先同步本地数据和元数据，再上传；上传和提交元数据都成功才清除 dirty。
- `Flush`：调用同一幂等提交路径；失败返回 `EIO`，使 `close(2)` 可见失败，dirty 保留。
- `Release`：只关闭 fd、释放句柄计数和锁；其返回值会被内核忽略，不能作为唯一写回点。
- 重复 `Flush` 可能发生，已提交且未再修改时必须直接成功，不重复上传。
- 新建零字节文件可留在本地并保持 dirty；当前协议不支持上传，`fsync` 返回 `EOPNOTSUPP`，`Flush` 返回 `EIO`。
- 不得把零字节文件标记为远端成功；用户写入至少一个字节后可再次同步。
- 上游资料称同名上传执行覆盖，但没有 expected-version/CAS；提交前检查不能消除检查后竞态。
- 因此不承诺跨客户端一致性，也不承诺其他客户端并发修改时绝不丢失更新。

## 5. 命名空间语义

- `Lookup/Readdir` 复用逐层分页、重名歧义和顶层 `pdir_key` 规则。
- `Create` 先创建 local-only 缓存记录；远端文件只在成功同步后存在。
- `Mkdir` 复用现有无覆盖创建；上游自动改名仍视为冲突。
- `Unlink` 对远端对象只调用 `Delete(..., false)`，即移入回收站；挂载不提供永久删除。
- local-only/dirty 文件删除后移入缓存恢复区并从挂载命名空间移除，不直接销毁最后一份本地数据。
- `Rmdir` 必须先列目录；非空返回 `ENOTEMPTY`，空目录才移入回收站。并发客户端仍可能造成检查后竞态。
- `Rename` 无论 flags 是否含 `RENAME_NOREPLACE` 都禁止覆盖；目标存在返回 `EEXIST`。
- 同目录改名和跨目录同名移动复用 `remote.Move`；跨目录同时改名返回 `EXDEV`，不拆成非原子多步操作。
- `RENAME_EXCHANGE`、`RENAME_WHITEOUT`、link、symlink 返回 `EOPNOTSUPP`。
- 挂载设置 `DisableXAttrs`；xattr 返回不支持/无数据，不能返回伪成功。

## 6. 建议接口

```go
type Version struct {
    FileID, ParentKey, Name string
    Size, MTime             int64
}

type Backend interface {
    List(context.Context, string) (remote.Listing, error)
    Resolve(context.Context, string) (remote.Entry, error)
    Download(context.Context, string, string) error
    PutFile(context.Context, string, *Version, io.ReaderAt, int64) (Version, error)
    Mkdir(context.Context, string) (string, error)
    Move(context.Context, string, string) error
    Trash(context.Context, string) error
}
```

- `PutFile` 的 `Version=nil` 表示创建并要求目标不存在；非 nil 表示提交既有文件并先做版本预检。
- `PutFile` 复用 `upload.Uploader.Upload` 的 `io.ReaderAt`，但绕过 CLI `Service.Upload` 的“目标存在即冲突”策略。
- 返回文件名与请求不一致仍报冲突；上传后重新 Resolve，形成新的缓存基线。
- `Download` 只写缓存中的唯一不存在路径，因此可复用当前安全下载器及其大小校验。
- `mount` 包通过单个 Backend 边界做 fake 测试，不让 FUSE 节点直接调用 MCP Client。

```go
type FileHandle struct {
    file *os.File
    // 所属文件状态、打开模式和本次会话是否修改
}

func (h *FileHandle) Read(context.Context, []byte, int64) (fuse.ReadResult, syscall.Errno)
func (h *FileHandle) Write(context.Context, []byte, int64) (uint32, syscall.Errno)
func (h *FileHandle) Fsync(context.Context, uint32) syscall.Errno
func (h *FileHandle) Flush(context.Context) syscall.Errno
func (h *FileHandle) Release(context.Context) syscall.Errno
```

## 7. go-fuse/v2 结论

- 当前可下载的最高稳定标签为 v2.12.1。
- `fs.FileReader.Read(ctx, dest, off)` 可返回 `fuse.ReadResultFd(fd, off, len(dest))`。
- Linux 默认启用可用的 splice 快路径；失败或短读会回退到 pread/pipe 修正，`DisableSplice` 可显式关闭。
- `ReadResultFd.Done()` 不关闭 fd；FileHandle 必须保证 fd 至少存活到读取响应发送完成，通常持续到 `Release`。
- 该优化只覆盖“本地缓存文件 fd → FUSE 设备”；TLS、哈希和 JSON/Base64 上传仍有转换，不能声称端到端零拷贝。
- `fs.Mount` 内部启动 Serve 并调用 `WaitMount`；停止使用 `Server.Unmount()`，成功后等待服务循环退出。

## 8. 前台停止

- `cmd/weiyun/main.go` 已提供 SIGINT/SIGTERM context；mount 执行器监听它并进入 stopping 状态。
- stopping 后拒绝新写打开，逐个提交本会话 dirty 文件；失败打印脱敏摘要并保留缓存，进程最终返回非零。
- 然后调用 `Server.Unmount()` 并 `Wait()`；若挂载忙则继续服务已有 close，不强制 lazy unmount、不提权。
- 只有卸载完成后释放缓存 flock；绝不删除用户指定缓存目录或其中 dirty 文件。

## 9. 探测结果与测试

```bash
GO=/root/.cache/tencent-weiyun-go/toolchain/go1.27.3/go/bin/go
$GO mod download -json github.com/hanwen/go-fuse/v2@v2.12.1
cd /tmp/tencent-weiyun-fuse-probe
CGO_ENABLED=0 $GO test -timeout=45s -count=1 -v .
```

- 环境：`/dev/fuse` 可读写，`fusermount3 3.17.2`；未提权。
- 结果：实际挂载成功，协议 7.45，完整/随机偏移读取 1 MiB+123 B 本地文件一致。
- `ReadResultFd` 路径运行成功；Fsync/Flush/Release 各触发一次；`Server.Unmount` 成功且无残留挂载。
- 环境无 strace，未直接观测 splice 系统调用，因此只确认能力可用，不声称本次一定命中 splice。
- 后续测试分层：纯状态机单测、mock Backend 测试、临时目录真实 FUSE 测试；全部禁止真实远端。

## 10. 已实施切片与文件范围

1. CLI 解析和帮助：`internal/cli/cli.go`、`internal/cli/cli_test.go`，保持旧命令完全不变。
2. 后端适配：新增 `internal/remote/mount.go` 及 mock 测试；不改 weiyun wire 类型。
3. 缓存与恢复：新增 `internal/mount/cache.go`、`cache_test.go`，先完成 flock、manifest、dirty 生命周期。
4. FUSE 只读切片：新增 `internal/mount/mount.go`、`node.go`、测试，完成 browse、整文件缓存和 fd 读取。
5. 写回切片：在同一 mount 文件中加入 create/write/truncate/fsync/flush/release，再加并发和崩溃恢复测试。
6. 命名空间切片：mkdir、rmdir、trash、rename-no-replace 和 unsupported errno 测试。
7. `go.mod/go.sum` 锁定 go-fuse v2.12.1，README 与 `docs/spec.md` 已同步，并执行全量 test/race/vet/static build。

实现采用单一 `Backend` 边界；单元测试、httptest 和 `WEIYUN_FUSE_TEST=1` 的本地 fakeBackend 真实 FUSE 测试均不读取用户配置或真实凭据。真实微云“同名上传覆盖”的兼容性仍未验证；该验证必须由用户单独批准并使用非重要测试文件。

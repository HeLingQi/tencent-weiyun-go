package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/HeLingQi/tencent-weiyun-go/internal/config"
	mountfs "github.com/HeLingQi/tencent-weiyun-go/internal/mount"
	"github.com/HeLingQi/tencent-weiyun-go/internal/remote"
	"github.com/HeLingQi/tencent-weiyun-go/internal/upload"
	"github.com/HeLingQi/tencent-weiyun-go/internal/weiyun"
	"golang.org/x/sys/unix"
)

const helpText = `微云纯 CLI

用法：
  weiyun [--config 路径] ls wy:/目录
  weiyun [--config 路径] mkdir wy:/目录
  weiyun [--config 路径] cp 本地文件 wy:/目录/文件
  weiyun [--config 路径] cp wy:/目录/文件 本地路径
  weiyun [--config 路径] mv wy:/旧路径 wy:/新路径
  weiyun [--config 路径] share wy:/文件或目录
  weiyun [--config 路径] rm wy:/文件或目录
  weiyun [--config 路径] rm -f wy:/文件或目录
  weiyun [--config 路径] mount wy:/目录 /空目录 [--cache-dir 路径]

配置：默认 ~/.config/weiyun/config.toml；可用 --config 显式指定 TOML 配置。
路径：wy:/ 是当前 Token 的授权根，不代表账号未授权区域。
安全：rm 默认进入回收站；rm -f 是不可恢复的永久删除，仍必须在真实终端输入 DELETE 确认。
参数：路径以 - 开头时，在路径参数前使用 --。
`

type Operations interface {
	List(context.Context, string) (remote.Listing, error)
	Mkdir(context.Context, string) (string, error)
	Upload(context.Context, string, string) error
	Download(context.Context, string, string) error
	Move(context.Context, string, string) error
	Share(context.Context, string) (string, error)
	Delete(context.Context, string, bool) error
}

type Dependencies struct {
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	IsTerminal        func() bool
	DefaultConfigPath func() (string, error)
	LoadConfig        func(string) (config.Config, error)
	NewOperations     func(config.Config) (Operations, error)
	Mount             func(context.Context, config.Config, string, string, string) error
}

type invocation struct {
	command   string
	arguments []string
	permanent bool
	cacheDir  string
}

func Run(ctx context.Context, args []string, dependencies Dependencies) int {
	dependencies = withDefaults(dependencies)
	configPath, help, remaining, err := parseGlobal(args)
	if err != nil {
		return fail(dependencies.Stderr, err)
	}
	if help || len(remaining) == 0 {
		_, _ = io.WriteString(dependencies.Stdout, helpText)
		return 0
	}
	call, err := parseInvocation(remaining)
	if err != nil {
		return fail(dependencies.Stderr, err)
	}
	if call.command == "rm" && call.permanent {
		normalized, err := remote.Normalize(call.arguments[0])
		if err != nil {
			return fail(dependencies.Stderr, err)
		}
		if normalized == "/" {
			return fail(dependencies.Stderr, remote.ErrRootOperation)
		}
		if !dependencies.IsTerminal() {
			return fail(dependencies.Stderr, errors.New("拒绝永久删除：stdin 不是真实交互终端"))
		}
		_, _ = fmt.Fprintf(dependencies.Stderr, "永久删除 wy:%s，不可恢复。输入 DELETE 确认: ", normalized)
		answer, err := bufio.NewReader(dependencies.Stdin).ReadString('\n')
		if err != nil {
			return fail(dependencies.Stderr, errors.New("拒绝永久删除：无法读取确认"))
		}
		if strings.TrimSpace(answer) != "DELETE" {
			return fail(dependencies.Stderr, errors.New("拒绝永久删除：未收到明确确认"))
		}
	}

	if configPath == "" {
		configPath, err = dependencies.DefaultConfigPath()
		if err != nil {
			return fail(dependencies.Stderr, err)
		}
	}
	cfg, err := dependencies.LoadConfig(configPath)
	if err != nil {
		return fail(dependencies.Stderr, err)
	}
	if call.command == "mount" {
		if err := dependencies.Mount(ctx, cfg, call.arguments[0], call.arguments[1], call.cacheDir); err != nil {
			return fail(dependencies.Stderr, err)
		}
		return 0
	}
	operations, err := dependencies.NewOperations(cfg)
	if err != nil {
		return fail(dependencies.Stderr, err)
	}
	if err := execute(ctx, operations, call, dependencies.Stdout); err != nil {
		return fail(dependencies.Stderr, err)
	}
	return 0
}

func withDefaults(dependencies Dependencies) Dependencies {
	if dependencies.Stdin == nil {
		dependencies.Stdin = os.Stdin
	}
	if dependencies.Stdout == nil {
		dependencies.Stdout = os.Stdout
	}
	if dependencies.Stderr == nil {
		dependencies.Stderr = os.Stderr
	}
	if dependencies.IsTerminal == nil {
		dependencies.IsTerminal = func() bool {
			_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
			return err == nil
		}
	}
	if dependencies.DefaultConfigPath == nil {
		dependencies.DefaultConfigPath = config.DefaultPath
	}
	if dependencies.LoadConfig == nil {
		dependencies.LoadConfig = config.Load
	}
	if dependencies.NewOperations == nil {
		dependencies.NewOperations = func(cfg config.Config) (Operations, error) {
			client, err := weiyun.NewClient(weiyun.DefaultEndpoint, cfg.Token, cfg.Cookie, nil)
			if err != nil {
				return nil, err
			}
			return remote.New(client, upload.New(client), nil), nil
		}
	}
	if dependencies.Mount == nil {
		dependencies.Mount = func(ctx context.Context, cfg config.Config, remotePath, mountpoint, cacheDir string) error {
			client, err := weiyun.NewClient(weiyun.DefaultEndpoint, cfg.Token, cfg.Cookie, nil)
			if err != nil {
				return err
			}
			backend := remote.New(client, upload.New(client), nil)
			backend.SetCacheTTL(60*time.Second, 15*time.Minute)
			return mountfs.Run(ctx, remotePath, mountpoint, cacheDir, cfg.Token, backend)
		}
	}
	return dependencies
}

func parseGlobal(args []string) (string, bool, []string, error) {
	var configPath string
	var remaining []string
	help := false
	afterSeparator := false
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !afterSeparator {
			switch {
			case argument == "--":
				afterSeparator = true
				remaining = append(remaining, argument)
				continue
			case argument == "--help" || argument == "-h":
				help = true
				continue
			case argument == "--config":
				if configPath != "" || i+1 >= len(args) || args[i+1] == "" {
					return "", false, nil, errors.New("--config 需要唯一的路径参数")
				}
				i++
				configPath = args[i]
				continue
			case strings.HasPrefix(argument, "--config="):
				if configPath != "" || strings.TrimPrefix(argument, "--config=") == "" {
					return "", false, nil, errors.New("--config 需要唯一的路径参数")
				}
				configPath = strings.TrimPrefix(argument, "--config=")
				continue
			}
		}
		remaining = append(remaining, argument)
	}
	return configPath, help, remaining, nil
}

func parseInvocation(args []string) (invocation, error) {
	command := args[0]
	if command == "mount" {
		operands, cacheDir, err := parseMountOperands(args[1:])
		if err != nil {
			return invocation{}, err
		}
		if len(operands) != 2 {
			return invocation{}, errors.New("mount 需要远程目录和挂载点")
		}
		if !strings.HasPrefix(operands[0], "wy:/") {
			return invocation{}, errors.New("mount 的第一个路径必须是 wy:/ 远程目录")
		}
		return invocation{command: command, arguments: operands, cacheDir: cacheDir}, nil
	}
	allowForce := command == "rm"
	operands, permanent, err := parseOperands(args[1:], allowForce)
	if err != nil {
		return invocation{}, err
	}
	expected := map[string]int{"ls": 1, "mkdir": 1, "cp": 2, "mv": 2, "share": 1, "rm": 1}
	count, ok := expected[command]
	if !ok {
		return invocation{}, fmt.Errorf("未知命令 %q", command)
	}
	if len(operands) != count {
		return invocation{}, fmt.Errorf("%s 需要 %d 个路径参数", command, count)
	}
	if command == "cp" {
		sourceRemote := strings.HasPrefix(operands[0], "wy:/")
		destinationRemote := strings.HasPrefix(operands[1], "wy:/")
		if sourceRemote == destinationRemote {
			return invocation{}, errors.New("cp 必须恰有一端是 wy:/ 远程路径")
		}
	}
	return invocation{command: command, arguments: operands, permanent: permanent}, nil
}

func parseMountOperands(args []string) ([]string, string, error) {
	var operands []string
	var cacheDir string
	afterSeparator := false
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !afterSeparator && argument == "--" {
			afterSeparator = true
			continue
		}
		if !afterSeparator && argument == "--cache-dir" {
			if cacheDir != "" || i+1 >= len(args) || args[i+1] == "" {
				return nil, "", errors.New("--cache-dir 需要唯一的路径参数")
			}
			i++
			cacheDir = args[i]
			continue
		}
		if !afterSeparator && strings.HasPrefix(argument, "--cache-dir=") {
			value := strings.TrimPrefix(argument, "--cache-dir=")
			if cacheDir != "" || value == "" {
				return nil, "", errors.New("--cache-dir 需要唯一的路径参数")
			}
			cacheDir = value
			continue
		}
		if !afterSeparator && strings.HasPrefix(argument, "-") {
			return nil, "", fmt.Errorf("未知参数 %q", argument)
		}
		operands = append(operands, argument)
	}
	return operands, cacheDir, nil
}

func parseOperands(args []string, allowForce bool) ([]string, bool, error) {
	var operands []string
	permanent := false
	afterSeparator := false
	for _, argument := range args {
		if !afterSeparator && argument == "--" {
			afterSeparator = true
			continue
		}
		if !afterSeparator && strings.HasPrefix(argument, "-") {
			if allowForce && argument == "-f" && !permanent {
				permanent = true
				continue
			}
			return nil, false, fmt.Errorf("未知参数 %q", argument)
		}
		operands = append(operands, argument)
	}
	return operands, permanent, nil
}

func execute(ctx context.Context, operations Operations, call invocation, stdout io.Writer) error {
	switch call.command {
	case "ls":
		listing, err := operations.List(ctx, call.arguments[0])
		if err != nil {
			return err
		}
		for _, dir := range listing.Dirs {
			_, _ = fmt.Fprintf(stdout, "d\t%s/\n", dir.DirName)
		}
		for _, file := range listing.Files {
			_, _ = fmt.Fprintf(stdout, "f\t%d\t%s\n", file.FileSize, file.Filename)
		}
		return nil
	case "mkdir":
		created, err := operations.Mkdir(ctx, call.arguments[0])
		if err == nil {
			_, _ = fmt.Fprintf(stdout, "wy:%s\n", created)
		}
		return err
	case "cp":
		if strings.HasPrefix(call.arguments[0], "wy:/") {
			return operations.Download(ctx, call.arguments[0], call.arguments[1])
		}
		return operations.Upload(ctx, call.arguments[0], call.arguments[1])
	case "mv":
		return operations.Move(ctx, call.arguments[0], call.arguments[1])
	case "share":
		link, err := operations.Share(ctx, call.arguments[0])
		if err == nil {
			_, _ = fmt.Fprintln(stdout, link)
		}
		return err
	case "rm":
		return operations.Delete(ctx, call.arguments[0], call.permanent)
	default:
		return errors.New("unknown command")
	}
}

func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "错误：%v\n", err)
	return 1
}

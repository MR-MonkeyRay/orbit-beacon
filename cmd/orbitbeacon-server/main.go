package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"orbitbeacon/internal/buildinfo"
	"orbitbeacon/internal/bundle"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/app"
	"orbitbeacon/internal/server/backup"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := runWithLogger(os.Args[1:], logger); err != nil {
		// 命令错误面向运维，直接输出到 CLI，不写结构化日志。
		fmt.Fprintln(os.Stderr, "orbitbeacon-server:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return runWithLogger(args, slog.New(slog.NewJSONHandler(os.Stderr, nil)))
}

func runWithLogger(args []string, logger *slog.Logger) error {
	if len(args) == 0 {
		return errors.New("缺少子命令：应为 serve、restore、version、health 或 verify-bundle")
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Fprintln(os.Stdout, "用法: orbitbeacon-server {serve|restore|version|health|verify-bundle} --help")
		return nil
	case "serve":
		return runServe(args[1:], logger)
	case "restore":
		return runRestore(args[1:])
	case "version":
		return runVersion(args[1:])
	case "health":
		return runHealth(args[1:])
	case "verify-bundle":
		return runVerifyBundle(args[1:])
	default:
		return fmt.Errorf("未知子命令 %q；应为 serve、restore、version、health 或 verify-bundle", args[0])
	}
}

func parseCommand(fs *flag.FlagSet, args []string) (bool, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, nil
		}
		return false, err
	}
	if fs.NArg() != 0 {
		return false, fmt.Errorf("多余的位置参数 %q", fs.Arg(0))
	}
	return true, nil
}

func runServe(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "JSON 配置文件")
	fs.String("data-dir", "", "本地持久化数据目录")
	fs.String("listen-addr", "127.0.0.1:8080", "HTTP 监听地址")
	fs.String("metrics-listen", "", "可选的独立 Prometheus 指标监听地址")
	fs.Bool("access-log", false, "每个请求写一条结构化访问日志")
	fs.Bool("allow-insecure-webhooks", false, "仅允许使用回环 IP 地址的 HTTP webhook URL")
	fs.Bool("notification-allow-private-networks", false, "允许通知访问私有网络")
	fs.String("public-url", "http://localhost:8080", "用于浏览器 Origin 校验的公开 HTTP 或 HTTPS URL")
	fs.Int("max-nodes", 500, "节点容量，须为正数，包含待处理和已禁用节点")
	fs.Int("web-update-interval-seconds", 10, "浏览器更新间隔（秒，2-60，重启后生效）")
	fs.String("trusted-proxies", "", "逗号分隔的可信代理 IP 地址或 CIDR 前缀")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	flags := make(map[string]string)
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "config" {
			flags[strings.ReplaceAll(f.Name, "-", "_")] = f.Value.String()
		}
	})
	cfg, err := config.LoadServer(config.LoadOptions{File: *configPath, Flags: flags})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.Build(cfg, logger, ctx)
	if err != nil {
		return err
	}
	return application.Run(ctx)
}

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "输出机器可读的构建信息")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	info := buildinfo.Current()
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	fmt.Fprintf(os.Stdout, "OrbitBeacon server %s (%s/%s, commit %s, built %s, %s)\n", info.Version, info.OS, info.Arch, info.Commit, info.BuildDate, info.GoVersion)
	return nil
}

func runHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	url := fs.String("url", "", "服务器 HTTP 或 HTTPS Origin")
	ca := fs.String("ca-file", "", "可选的 CA PEM 证书包")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	return checkHealth(*url, *ca)
}

func runVerifyBundle(args []string) error {
	fs := flag.NewFlagSet("verify-bundle", flag.ContinueOnError)
	directory := fs.String("directory", "", "已签名的发布包目录")
	key := fs.String("public-key", "", "Ed25519 公钥文件，支持 PEM 或原始字节格式")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	manifest, err := bundle.VerifyBundle(*directory, *key)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "发布包 %s 校验通过，共 %d 个产物\n", manifest.Version, len(manifest.Artifacts))
	return nil
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "本地持久化数据目录")
	input := fs.String("input", "", "待恢复的归档文件")
	replace := fs.Bool("replace", false, "替换现有数据目录中选定的类别")
	passwordFile := fs.String("password-file", "", "归当前用户所有、权限为 0600 的归档密码文件")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	if *dataDir == "" || *input == "" {
		return errors.New("restore 需要 --data-dir 和 --input")
	}
	password, err := readRestorePassword(*passwordFile)
	if err != nil {
		return err
	}
	result, err := backup.Restore(context.Background(), *dataDir, *input, password, *replace)
	password = ""
	if err != nil {
		var detail *backup.DependencyConflict
		if errors.As(err, &detail) {
			return fmt.Errorf("restore_dependency_conflict: %s", detail)
		}
		return err
	}
	fmt.Fprintln(os.Stdout, "已恢复归档", result.ArchiveID)
	fmt.Fprintln(os.Stdout, "回滚文件:", result.RollbackPath)
	return nil
}

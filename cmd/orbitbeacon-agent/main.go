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
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"orbitbeacon/internal/agent/filelock"
	"orbitbeacon/internal/agent/service"
	"orbitbeacon/internal/buildinfo"
	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

const exitIdentityConflict = 3

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var failure *service.HTTPError
	if errors.As(err, &failure) && failure.StatusCode == 409 && failure.Code == "identity_conflict" {
		return exitIdentityConflict
	}
	return 1
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := runWithLogger(os.Args[1:], logger); err != nil {
		// 命令错误面向运维，直接输出到 CLI，不写结构化日志。
		fmt.Fprintln(os.Stderr, "orbitbeacon-agent:", err)
		os.Exit(exitCode(err))
	}
}

func run(args []string) error {
	return runWithLogger(args, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

func runWithLogger(args []string, logger *slog.Logger) error {
	if len(args) == 0 {
		return errors.New("缺少子命令：应为 run、enroll、doctor 或 version")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stdout, "用法: orbitbeacon-agent {run|enroll|doctor|version} --help")
		return nil
	}
	switch args[0] {
	case "version":
		return runVersion(args[1:])
	case "run":
		return runAgent(args[1:], logger)
	case "enroll":
		return runEnroll(args[1:], logger)
	case "doctor":
		return runDoctor(args[1:], logger)
	default:
		return fmt.Errorf("未知子命令 %q", args[0])
	}
}

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "输出机器可读的构建信息")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("多余的位置参数 %q", fs.Arg(0))
	}
	info := buildinfo.Current()
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	fmt.Fprintf(os.Stdout, "OrbitBeacon agent %s (%s/%s, commit %s, built %s, %s)\n", info.Version, info.OS, info.Arch, info.Commit, info.BuildDate, info.GoVersion)
	return nil
}

type runtimeCommand struct {
	ctx       context.Context
	cancel    context.CancelFunc
	loadAgent func() (config.Agent, error)
	cfg       config.Agent
}

func prepareRuntimeCommand(name string, args []string, configure func(*flag.FlagSet) error) (*runtimeCommand, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	file := fs.String("config", "", "Agent 的 JSON 配置文件")
	policyPath := fs.String("install-policy", "", "归管理员所有、运行时不可修改的安装策略 JSON 文件")
	runtimeFlags := map[string]*string{
		"report_interval_seconds": fs.String("report-interval-seconds", "", "覆盖指标上报间隔（秒）"),
		"disk_interval_seconds":   fs.String("disk-interval-seconds", "", "覆盖磁盘采集间隔（秒）"),
		"interfaces":              fs.String("interfaces", "", "用 JSON 字符串数组覆盖受监控网卡"),
		"disk_paths":              fs.String("disk-paths", "", "用 JSON 字符串数组覆盖受监控磁盘路径"),
		"probe_targets":           fs.String("probe-targets", "", "用 JSON 字符串数组覆盖探测目标"),
	}
	if configure != nil {
		if err := configure(fs); err != nil {
			return nil, err
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("多余的位置参数 %q", fs.Arg(0))
	}
	if *policyPath == "" {
		return nil, errors.New("必须提供 --install-policy；不会自动推断信任策略")
	}
	policy, err := config.ReadInstallPolicy(*policyPath, 0)
	if err != nil {
		return nil, err
	}
	flags := make(map[string]string)
	fs.Visit(func(f *flag.Flag) {
		key := strings.ReplaceAll(f.Name, "-", "_")
		if value, ok := runtimeFlags[key]; ok {
			flags[key] = *value
		}
	})
	loadAgent, err := agentLoader(*file, flags, policy)
	if err != nil {
		return nil, err
	}
	cfg, err := loadAgent()
	if err != nil {
		return nil, err
	}
	if err := validateRuntimeIdentity(cfg); err != nil {
		return nil, err
	}
	if err := os.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin"); err != nil {
		return nil, err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return &runtimeCommand{ctx: ctx, cancel: cancel, loadAgent: loadAgent, cfg: cfg}, nil
}

func (c *runtimeCommand) close() { c.cancel() }

func validateRuntimeIdentity(cfg config.Agent) error {
	if runtime.GOOS == "windows" {
		return errors.New("runtime_identity_unsupported: 未实现 Windows 账户强制校验，未执行任何运行时命令")
	}
	if os.Geteuid() == 0 || os.Getegid() == 0 || os.Geteuid() != cfg.ExpectedUID || os.Getegid() != cfg.ExpectedGID {
		return errors.New("runtime_identity_mismatch: 须以安装策略固定的非 root UID/GID 运行")
	}
	if cfg.ExpectedAccount != "" {
		account, err := user.LookupId(fmt.Sprint(os.Geteuid()))
		if err != nil || account.Username != cfg.ExpectedAccount {
			return errors.New("runtime_identity_mismatch: 当前账户与安装策略不一致")
		}
	}
	return nil
}

func runAgent(args []string, logger *slog.Logger) error {
	reload, stopReload := reloadSignals()
	defer stopReload()
	command, err := prepareRuntimeCommand("run", args, nil)
	if err != nil || command == nil {
		return err
	}
	defer command.close()
	unlock, err := lockRun(command.ctx, command.cfg.StateDir)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	defer unlock()
	agent, err := service.OpenRuntime(command.ctx, command.cfg, logger)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	defer agent.Close()
	err = agent.Run(command.ctx, reload, func() (v1.RuntimeSettings, error) {
		next, err := command.loadAgent()
		return next.RuntimeSettings, err
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func runEnroll(args []string, logger *slog.Logger) error {
	var tokenFile string
	command, err := prepareRuntimeCommand("enroll", args, func(fs *flag.FlagSet) error {
		fs.StringVar(&tokenFile, "token-file", "", "可选的注册票据文件；省略则找回已保存的节点凭据")
		return nil
	})
	if err != nil || command == nil {
		return err
	}
	defer command.close()
	agent, err := service.OpenRuntime(command.ctx, command.cfg, logger)
	if err != nil {
		return err
	}
	defer agent.Close()
	requestCtx, stop := context.WithTimeout(command.ctx, 5*time.Second)
	defer stop()
	result, err := runEnrollment(requestCtx, tokenFile, agent.Enroll)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "节点 %s 注册成功\n", result.NodeID)
	return nil
}

func runDoctor(args []string, logger *slog.Logger) error {
	var asJSON bool
	command, err := prepareRuntimeCommand("doctor", args, func(fs *flag.FlagSet) error {
		fs.BoolVar(&asJSON, "json", false, "输出机器可读的诊断结果")
		return nil
	})
	if err != nil || command == nil {
		return err
	}
	defer command.close()
	agent, err := service.OpenRuntime(command.ctx, command.cfg, logger)
	if err != nil {
		return err
	}
	defer agent.Close()
	result, checkErr := agent.Doctor(command.ctx)
	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stdout, "配置: %s\n身份文件: %s\n连通性: %s\n节点认证: %s\n采集: %s\n安装模式: %s\n", result.Configuration, result.Identity, result.Connectivity, result.Authentication, result.Collection, result.InstallMode)
		for metric, status := range result.MetricStatus {
			fmt.Fprintf(os.Stdout, "%s: %s\n", metric, status)
		}
	}
	return checkErr
}

// 启动时固定已校验的策略、环境变量和显式命令行参数。重载只重新读取同一个
// 本地 JSON 文件，信任根不随进程环境变化。
func agentLoader(file string, flags map[string]string, policy config.InstallPolicy) (func() (config.Agent, error), error) {
	if file != "" {
		var err error
		file, err = filepath.Abs(file)
		if err != nil {
			return nil, err
		}
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "ORBITBEACON_") {
			environment[key] = value
		}
	}
	overrides := make(map[string]string, len(flags))
	for key, value := range flags {
		overrides[key] = value
	}
	opts := config.LoadOptions{File: file, Flags: overrides, LookupEnv: func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	}}
	return func() (config.Agent, error) { return config.LoadAgent(opts, &policy) }, nil
}

func lockRun(ctx context.Context, stateDir string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, errors.New("无法准备 Agent 状态目录以创建运行锁")
	}
	return filelock.TryAcquire(ctx, filepath.Join(stateDir, "run.lock"))
}

// runEnrollment 在任何未确认结果下都保留票据。清理失败时同样返回已确认的
// 响应，调用方据此可区分本地清理失败与注册未生效。
func runEnrollment(ctx context.Context, tokenFile string, enroll func(context.Context, string) (v1.EnrollmentResponse, error)) (v1.EnrollmentResponse, error) {
	if tokenFile == "" {
		return enroll(ctx, "")
	}
	ticket, err := openEnrollmentTicket(tokenFile)
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	defer ticket.close()
	result, err := enroll(ctx, ticket.token)
	if err != nil {
		return result, err
	}
	if result.NodeID == "" {
		return result, errors.New("注册响应无效；已保留注册票据")
	}
	if err := ticket.remove(); err != nil {
		return result, errors.New("注册成功但清理注册票据失败；请检查原目录并安全删除残留票据或 .orbitbeacon-ticket-* 恢复文件")
	}
	return result, nil
}

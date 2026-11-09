package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

func runStorage(args []string) error {
	if len(args) == 0 {
		return errors.New("storage 需要子命令：status、preview、run 或 disable")
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Fprintln(os.Stdout, "用法: orbitbeacon-server storage {status|preview|run|disable} --config <file>；preview/run 须停服，使用 --help 查看确认参数")
		return nil
	case "status":
		return runStorageStatus(args[1:])
	case "preview", "run":
		return runStorageRecovery(args[0], args[1:])
	case "disable":
		return runStorageDisable(args[1:])
	default:
		return fmt.Errorf("未知 storage 子命令 %q；应为 status、preview、run 或 disable", args[0])
	}
}

func runStorageDisable(args []string) error {
	fs := flag.NewFlagSet("storage disable", flag.ContinueOnError)
	configPath := fs.String("config", "", "YAML 配置文件")
	dataDir := fs.String("data-dir", "", "本地持久化数据目录")
	confirmation := fs.String("confirm", "", "精确的集群 UUID；仅主库健康且为当前权威时可清退主备")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	if *confirmation == "" {
		return errors.New("storage disable 需要 --confirm <cluster-id>")
	}
	cfg, err := offlineConfig(fs, *configPath, *dataDir)
	if err != nil {
		return err
	}
	lock, err := datadir.Lock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	ctx := context.Background()
	if err = backup.RecoverRestore(ctx, cfg); err != nil {
		return err
	}
	if err = storage.DisableReplication(ctx, cfg.Database, cfg.DataDir, *confirmation); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "已清退主备配置，保留两端数据；现在可移除 standby 配置。")
	return nil
}

func runStorageStatus(args []string) error {
	fs := flag.NewFlagSet("storage status", flag.ContinueOnError)
	configPath := fs.String("config", "", "YAML 配置文件")
	dataDir := fs.String("data-dir", "", "本地持久化数据目录")
	asJSON := fs.Bool("json", false, "输出机器可读的状态；observed_at 不是远端健康证明")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	cfg, err := offlineConfig(fs, *configPath, *dataDir)
	if err != nil {
		return err
	}
	status, err := storage.ReadRecoveryStatus(context.Background(), cfg.Database, cfg.DataDir)
	if err != nil {
		return err
	}
	cluster := ""
	if cfg.Database.Standby != nil {
		cluster, err = storage.ReplicationClusterID(context.Background(), cfg.DataDir)
		if err != nil {
			return err
		}
	}
	if *asJSON {
		return writeStorageJSON(struct {
			storage.RecoveryStatus
			ClusterID string `json:"cluster_id,omitempty"`
		}{status, cluster})
	}
	fmt.Fprintln(os.Stdout, "observed_at:", status.ObservedAt.Format(time.RFC3339Nano))
	if cluster != "" {
		fmt.Fprintln(os.Stdout, "集群:", cluster)
	}
	fmt.Fprintln(os.Stdout, "主备已启用:", status.Enabled)
	for _, domain := range status.Domains {
		role := "无"
		if domain.ActiveRole != nil {
			role = *domain.ActiveRole
		}
		fmt.Fprintf(os.Stdout, "%s: state=%s active=%s replication=%s reason=%s\n", domain.Domain, domain.State, role, domain.ReplicationStatus, domain.ReasonCode)
		fmt.Fprintf(os.Stdout, "  primary: %s instance=%s reachable=%t ready=%t\n", domain.Primary.Label, domain.Primary.InstanceID, domain.Primary.Reachable, domain.Primary.Ready)
		if domain.Standby != nil {
			fmt.Fprintf(os.Stdout, "  standby: %s instance=%s reachable=%t ready=%t\n", domain.Standby.Label, domain.Standby.InstanceID, domain.Standby.Reachable, domain.Standby.Ready)
		}
		if domain.ProvenLossSeconds == nil {
			fmt.Fprintln(os.Stdout, "  proven_loss_seconds: 未知，未验证零丢失")
		} else {
			fmt.Fprintf(os.Stdout, "  proven_loss_seconds: %g\n", *domain.ProvenLossSeconds)
		}
		if domain.ManualDeadlineAt != nil {
			fmt.Fprintln(os.Stdout, "  manual_deadline_at:", domain.ManualDeadlineAt.Format(time.RFC3339Nano))
		}
		for _, action := range domain.AvailableActions {
			fmt.Fprintf(os.Stdout, "  可用动作: action=%s target=%s\n", action.Action, action.Target)
		}
		if domain.Operation != nil {
			writeStorageOperation(domain.Operation)
		}
	}
	return nil
}

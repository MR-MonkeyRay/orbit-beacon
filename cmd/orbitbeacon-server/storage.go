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
		return errors.New("storage 需要子命令：status 或 disable")
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Fprintln(os.Stdout, "用法: orbitbeacon-server storage {status|disable} --config <file> [--confirm <cluster-id>]")
		return nil
	case "status":
		return runStorageStatus(args[1:])
	case "disable":
		return runStorageDisable(args[1:])
	default:
		return fmt.Errorf("未知 storage 子命令 %q；应为 status 或 disable", args[0])
	}
}

func runStorageDisable(args []string) error {
	fs := flag.NewFlagSet("storage disable", flag.ContinueOnError)
	configPath := fs.String("config", "", "JSON 配置文件")
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
	configPath := fs.String("config", "", "JSON 配置文件")
	dataDir := fs.String("data-dir", "", "本地持久化数据目录")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	cfg, err := offlineConfig(fs, *configPath, *dataDir)
	if err != nil {
		return err
	}
	cluster, err := storage.ReplicationClusterID(context.Background(), cfg.DataDir)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "集群:", cluster)
	fmt.Fprintln(os.Stdout, "observed_at:", time.Now().UTC().Format(time.RFC3339Nano))
	return nil
}

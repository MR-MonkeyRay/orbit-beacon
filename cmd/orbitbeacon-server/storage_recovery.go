package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

func runStorageRecovery(action string, args []string) error {
	fs := flag.NewFlagSet("storage "+action, flag.ContinueOnError)
	configPath := fs.String("config", "", "YAML 配置文件")
	dataDir := fs.String("data-dir", "", "本地持久化数据目录")
	asJSON := fs.Bool("json", false, "输出机器可读的预览或终态")
	var domain, operationAction, target, operation, confirmation string
	var acceptLoss bool
	if action == "preview" {
		fs.StringVar(&domain, "domain", "", "逻辑域：main 或 history")
		fs.StringVar(&operationAction, "action", "", "恢复动作：rebuild、resume 或 promote")
		fs.StringVar(&target, "target", "", "目标角色：primary 或 standby")
	} else {
		fs.StringVar(&operation, "operation", "", "preview 返回的操作 UUID")
		fs.StringVar(&confirmation, "confirm", "", "精确的 domain/target/instance_uuid 确认值")
		fs.BoolVar(&acceptLoss, "accept-data-loss", false, "明确接受人工提升可能丢失已提交数据")
	}
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	if action == "preview" && (domain == "" || operationAction == "" || target == "") {
		return errors.New("storage preview 需要 --domain、--action 和 --target")
	}
	if action == "run" && (operation == "" || confirmation == "") {
		return errors.New("storage run 需要 --operation 和 --confirm")
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = backup.RecoverRestore(ctx, cfg); err != nil {
		return err
	}
	databases, err := storage.OpenRecoveryDatabases(ctx, cfg.Database, cfg.DataDir)
	if err != nil {
		return err
	}
	defer databases.Close()
	if action == "preview" {
		preview, err := databases.PreviewRecovery(ctx, "local", storage.RecoveryRequest{Domain: storage.Domain(domain), Action: operationAction, Target: target})
		if err != nil {
			return err
		}
		if *asJSON {
			return writeStorageJSON(preview)
		}
		fmt.Fprintln(os.Stdout, "操作:", preview.OperationID)
		fmt.Fprintln(os.Stdout, "到期:", preview.ExpiresAt.Format(time.RFC3339Nano))
		if preview.Source != nil {
			fmt.Fprintln(os.Stdout, "保留源:", preview.Source.Label, preview.Source.InstanceID)
		}
		fmt.Fprintln(os.Stdout, "目标:", preview.Target.Label, preview.Target.InstanceID)
		fmt.Fprintln(os.Stdout, "精确确认值:", preview.Confirmation)
		if preview.RequiresDataLossAcceptance {
			fmt.Fprintln(os.Stdout, "此操作可能丢失已提交数据；run 必须显式指定 --accept-data-loss。")
		}
		return nil
	}
	record, err := databases.AcceptRecovery(ctx, "local", operation, confirmation, acceptLoss)
	if err != nil {
		return err
	}
	if record.Status != "succeeded" && record.Status != "failed" {
		if err = databases.StartReplication(ctx); err != nil {
			return err
		}
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for record.Status != "succeeded" && record.Status != "failed" {
			select {
			case <-ctx.Done():
				return fmt.Errorf("命令已停止等待；操作记录保留，下次运行服务器或 storage run 后继续协调，请用 storage status 核实：%w", ctx.Err())
			case <-ticker.C:
			}
			record, err = databases.RecoveryOperation(ctx, "local", operation)
			if err != nil {
				return err
			}
		}
	}
	if *asJSON {
		if err = writeStorageJSON(record); err != nil {
			return err
		}
	} else {
		writeStorageOperation(&record)
	}
	if record.Status == "failed" {
		return fmt.Errorf("恢复操作失败：%s", record.ErrorCode)
	}
	return nil
}

func writeStorageJSON(value any) error {
	output := json.NewEncoder(os.Stdout)
	output.SetIndent("", "  ")
	return output.Encode(value)
}

func writeStorageOperation(operation *storage.RecoveryOperation) {
	phase := "尚未执行"
	if operation.Phase != nil {
		phase = *operation.Phase
	}
	fmt.Fprintf(os.Stdout, "操作: %s action=%s target=%s status=%s phase=%s rows_copied=%d error_code=%s\n", operation.ID, operation.Action, operation.Target, operation.Status, phase, operation.RowsCopied, operation.ErrorCode)
}

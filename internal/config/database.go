package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type DatabaseConfig struct {
	Primary  DatabaseEndpoint  `json:"primary"`
	Standby  *DatabaseEndpoint `json:"standby,omitempty"`
	Failover FailoverConfig    `json:"failover"`
}

func (d DatabaseConfig) MarshalJSON() ([]byte, error) {
	var failover *FailoverConfig
	if d.Standby != nil {
		failover = &d.Failover
	}
	return json.Marshal(struct {
		Primary  DatabaseEndpoint  `json:"primary"`
		Standby  *DatabaseEndpoint `json:"standby,omitempty"`
		Failover *FailoverConfig   `json:"failover,omitempty"`
	}{d.Primary, d.Standby, failover})
}

type DatabaseEndpoint struct {
	Driver          string       `json:"driver,omitempty"`
	Network         string       `json:"network,omitempty"`
	Address         string       `json:"address,omitempty"`
	User            string       `json:"user,omitempty"`
	PasswordFile    string       `json:"password_file,omitempty"`
	MainDatabase    string       `json:"main_database,omitempty"`
	HistoryDatabase string       `json:"history_database,omitempty"`
	TLS             *DatabaseTLS `json:"tls,omitempty"`
}

func (endpoint *DatabaseEndpoint) UnmarshalJSON(data []byte) error {
	type plain DatabaseEndpoint
	var raw plain
	if err := decodeDatabaseJSON(data, &raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("数据库端点必须是对象")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("数据库端点字段不能为 null")
		}
	}
	if _, supplied := fields["driver"]; supplied && raw.Driver == "" {
		return errors.New("数据库 driver 不能为空")
	}
	if raw.Driver == "" || raw.Driver == "sqlite" {
		for key := range fields {
			if key != "driver" {
				return errors.New("SQLite 不接受 MySQL 字段或自定义路径")
			}
		}
	}
	*endpoint = DatabaseEndpoint(raw)
	return nil
}

type DatabaseTLS struct {
	ServerName string `json:"server_name,omitempty"`
	CAFile     string `json:"ca_file,omitempty"`
}

func (tls *DatabaseTLS) UnmarshalJSON(data []byte) error {
	type plain DatabaseTLS
	var raw plain
	if err := decodeDatabaseJSON(data, &raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("database TLS 必须是对象")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("database TLS 字段不能为 null")
		}
	}
	*tls = DatabaseTLS(raw)
	return nil
}

type FailoverConfig struct {
	FailureTimeout time.Duration `json:"-"`
	MaxLossWindow  time.Duration `json:"-"`
	RecoveryStable time.Duration `json:"-"`
	ManualAfter    time.Duration `json:"-"`
}

func (f FailoverConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"failure_timeout": f.FailureTimeout.String(), "max_loss_window": f.MaxLossWindow.String(), "recovery_stable": f.RecoveryStable.String(), "manual_after": f.ManualAfter.String()})
}

func (f *FailoverConfig) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("database.failover 必须是对象")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return errors.New("database.failover 必须是对象")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("database.failover 时间不能为 null")
		}
	}
	var raw struct {
		FailureTimeout *string `json:"failure_timeout"`
		MaxLossWindow  *string `json:"max_loss_window"`
		RecoveryStable *string `json:"recovery_stable"`
		ManualAfter    *string `json:"manual_after"`
	}
	if err := decodeDatabaseJSON(data, &raw); err != nil {
		return err
	}
	*f = defaultFailover()
	for _, field := range []struct {
		name   string
		raw    *string
		target *time.Duration
	}{
		{"failure_timeout", raw.FailureTimeout, &f.FailureTimeout}, {"max_loss_window", raw.MaxLossWindow, &f.MaxLossWindow}, {"recovery_stable", raw.RecoveryStable, &f.RecoveryStable}, {"manual_after", raw.ManualAfter, &f.ManualAfter},
	} {
		if field.raw == nil {
			continue
		}
		duration, err := time.ParseDuration(*field.raw)
		if err != nil {
			return fmt.Errorf("database.failover.%s 时间无效", field.name)
		}
		*field.target = duration
	}
	return validateFailover(*f)
}

func defaultFailover() FailoverConfig {
	return FailoverConfig{FailureTimeout: 30 * time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: 60 * time.Second, ManualAfter: 24 * time.Hour}
}

func decodeDatabaseJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("database 配置结构无效")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("database 配置必须是单个对象")
	}
	return nil
}

func (d *DatabaseConfig) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("database 必须是对象")
	}
	var raw struct {
		Primary  json.RawMessage `json:"primary"`
		Standby  json.RawMessage `json:"standby"`
		Failover json.RawMessage `json:"failover"`
	}
	if err := decodeDatabaseJSON(data, &raw); err != nil {
		return err
	}
	next := DatabaseConfig{Primary: DatabaseEndpoint{Driver: "sqlite"}, Failover: defaultFailover()}
	if len(raw.Primary) > 0 {
		if bytes.Equal(bytes.TrimSpace(raw.Primary), []byte("null")) {
			return errors.New("database.primary 必须是对象")
		}
		if err := decodeDatabaseJSON(raw.Primary, &next.Primary); err != nil {
			return err
		}
	}
	if len(raw.Standby) > 0 {
		if bytes.Equal(bytes.TrimSpace(raw.Standby), []byte("null")) || bytes.Equal(bytes.TrimSpace(raw.Standby), []byte("{}")) {
			return errors.New("database.standby 不能为空")
		}
		next.Standby = new(DatabaseEndpoint)
		if err := decodeDatabaseJSON(raw.Standby, next.Standby); err != nil {
			return err
		}
		if next.Standby.Driver == "" {
			return errors.New("database.standby 必须指定 driver")
		}
	}
	if len(raw.Failover) > 0 {
		if next.Standby == nil {
			return errors.New("database.failover 需要 standby")
		}
		if err := json.Unmarshal(raw.Failover, &next.Failover); err != nil {
			return err
		}
	}
	if err := normalizeEndpoint(&next.Primary); err != nil {
		return fmt.Errorf("database.primary: %w", err)
	}
	if next.Standby != nil {
		if next.Primary.Driver != "mysql" {
			return errors.New("database.standby 仅支持 MySQL 主库")
		}
		if err := normalizeEndpoint(next.Standby); err != nil {
			return fmt.Errorf("database.standby: %w", err)
		}
	}
	*d = next
	return nil
}

var databaseNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func normalizeEndpoint(endpoint *DatabaseEndpoint) error {
	if endpoint.Driver == "" {
		endpoint.Driver = "sqlite"
	}
	switch endpoint.Driver {
	case "sqlite":
		if endpoint.Network != "" || endpoint.Address != "" || endpoint.User != "" || endpoint.PasswordFile != "" || endpoint.MainDatabase != "" || endpoint.HistoryDatabase != "" || endpoint.TLS != nil {
			return errors.New("SQLite 不接受 MySQL 字段或自定义路径")
		}
	case "mysql":
		if endpoint.Network == "" {
			endpoint.Network = "tcp"
		}
		if strings.TrimSpace(endpoint.User) == "" || endpoint.PasswordFile == "" {
			return errors.New("MySQL 需要 user 和 password_file")
		}
		if err := ValidatePath(endpoint.PasswordFile); err != nil {
			return errors.New("password_file 路径无效")
		}
		if endpoint.MainDatabase == "" {
			endpoint.MainDatabase = "orbitbeacon"
		}
		if endpoint.HistoryDatabase == "" {
			endpoint.HistoryDatabase = "orbitbeacon_history"
		}
		if !databaseNamePattern.MatchString(endpoint.MainDatabase) || !databaseNamePattern.MatchString(endpoint.HistoryDatabase) || endpoint.MainDatabase == endpoint.HistoryDatabase {
			return errors.New("MySQL 数据库名无效或重复")
		}
		switch endpoint.Network {
		case "tcp":
			host, port, err := net.SplitHostPort(endpoint.Address)
			number, parseErr := strconv.ParseUint(port, 10, 16)
			if err != nil || host == "" || parseErr != nil || number == 0 {
				return errors.New("MySQL TCP address 必须是 host:port")
			}
			if endpoint.TLS != nil {
				if endpoint.TLS.ServerName == "" {
					endpoint.TLS.ServerName = host
				}
				if strings.ContainsAny(endpoint.TLS.ServerName, "\x00\r\n") {
					return errors.New("TLS server_name 无效")
				}
				if endpoint.TLS.CAFile != "" {
					if err := ValidatePath(endpoint.TLS.CAFile); err != nil {
						return errors.New("TLS ca_file 路径无效")
					}
				}
			}
		case "unix":
			if !filepath.IsAbs(endpoint.Address) || strings.ContainsRune(endpoint.Address, 0) || endpoint.TLS != nil {
				return errors.New("MySQL Unix socket 需要绝对路径且不能配置 TLS")
			}
		default:
			return errors.New("MySQL network 必须是 tcp 或 unix")
		}
	default:
		return errors.New("数据库 driver 必须是 sqlite 或 mysql")
	}
	return nil
}

func validateFailover(f FailoverConfig) error {
	if f.FailureTimeout < time.Second || f.RecoveryStable < time.Second || f.MaxLossWindow < 0 || f.ManualAfter <= f.FailureTimeout {
		return errors.New("database.failover 时间范围无效")
	}
	return nil
}

func (d DatabaseConfig) Validate() error {
	if err := normalizeEndpoint(&d.Primary); err != nil {
		return err
	}
	if d.Standby != nil {
		standby := *d.Standby
		if d.Primary.Driver != "mysql" {
			return errors.New("database.standby 仅支持 MySQL 主库")
		}
		if err := normalizeEndpoint(&standby); err != nil {
			return err
		}
		if err := validateFailover(d.Failover); err != nil {
			return err
		}
	}
	return nil
}

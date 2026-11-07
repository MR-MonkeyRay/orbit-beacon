// Package notify 管理加密通道与持久化的通知投递。
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

var (
	ErrNotFound         = errors.New("notification channel not found")
	ErrCapacity         = errors.New("maximum of 20 notification channels reached")
	ErrUnavailable      = errors.New("notification secrets unavailable")
	ErrSecretUnreadable = errors.New("notification secret unreadable")
)

type Config struct {
	URL         string   `json:"url,omitempty"` // 只写字段；仅在内部填充，用于校验和发送。
	URLRedacted string   `json:"url_redacted,omitempty"`
	URLSet      bool     `json:"url_set,omitempty"`
	ChatID      string   `json:"chat_id,omitempty"`
	Host        string   `json:"host,omitempty"`
	Port        int      `json:"port,omitempty"`
	Username    string   `json:"username,omitempty"`
	From        string   `json:"from,omitempty"`
	To          []string `json:"to,omitempty"`
	Template    string   `json:"template,omitempty"`
}
type Channel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Enabled   bool      `json:"enabled"`
	Config    Config    `json:"config"`
	SecretSet bool      `json:"secret_set"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MarshalJSON 保证仅投递用的 URL 内容不出现在任何通道响应中。
func (c Channel) MarshalJSON() ([]byte, error) {
	type channel Channel
	c.Config.URL = ""
	return json.Marshal(channel(c))
}

// Secret 只写：按通道类型分别是签名密钥、机器人令牌或 SMTP 密码。
// 省略则保留原值，空字符串则清除；kind 创建后不可变。
type Patch struct {
	Name    *string         `json:"name,omitempty"`
	Kind    *string         `json:"kind,omitempty"`
	Enabled *bool           `json:"enabled,omitempty"`
	Config  json.RawMessage `json:"config,omitempty"`
	Secret  json.RawMessage `json:"secret,omitempty"`
}
type Options struct {
	AllowInsecureWebhooks bool
	AllowPrivateNetworks  bool
	Now                   func() time.Time
	Logger                *slog.Logger
}
type Service struct {
	db             *storage.Database
	box            *secretbox.Box
	opts           Options
	sender         *sender
	mu             sync.Mutex
	closed         atomic.Bool
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan struct{}
	stopCtx        context.Context
	stop           context.CancelFunc
	secretWarnings map[string]time.Time
	logger         *slog.Logger
}

func New(db *storage.Database, box *secretbox.Box, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	stopCtx, stop := context.WithCancel(context.Background())
	return &Service{db: db, box: box, opts: opts, sender: newSender(opts), done: make(chan struct{}), wake: make(chan struct{}, 1), stopCtx: stopCtx, stop: stop, secretWarnings: make(map[string]time.Time), logger: opts.Logger}
}

var chatIDPattern = regexp.MustCompile(`^(?:-?[0-9]{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$`)
var tokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

func (s *Service) validate(c Channel, secret string) error {
	return ValidateChannel(c, secret, s.opts.AllowInsecureWebhooks)
}

// ValidateChannel 仅检查通道语法；不执行 DNS 或 I/O。
func ValidateChannel(c Channel, secret string, allowInsecureWebhooks bool) error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 100 {
		return validation.Invalid("name", "must contain 1..100 UTF-8 bytes")
	}
	cfg := c.Config
	if !validTemplate(cfg.Template) {
		return validation.Invalid("config", "invalid message template")
	}
	switch c.Kind {
	case "webhook":
		if cfg.Template != "" || cfg.ChatID != "" || cfg.Host != "" || cfg.Port != 0 || cfg.Username != "" || cfg.From != "" || len(cfg.To) > 0 {
			return validation.Invalid("config", "unexpected fields for webhook")
		}
		if cfg.URL == "" && cfg.URLSet {
			break
		}
		if _, code := destinationURL(c.Kind, cfg.URL, allowInsecureWebhooks); code != "" {
			return validation.Invalid("config", "invalid channel URL")
		}
	case "feishu", "dingtalk", "wecom", "bark", "ntfy":
		if cfg.ChatID != "" || cfg.Host != "" || cfg.Port != 0 || cfg.Username != "" || cfg.From != "" || len(cfg.To) > 0 {
			return validation.Invalid("config", "unexpected fields for URL channel")
		}
		if !(cfg.URL == "" && cfg.URLSet) {
			if _, code := destinationURL(c.Kind, cfg.URL, allowInsecureWebhooks); code != "" {
				return validation.Invalid("config", "invalid channel URL")
			}
		}
		if !validIMSecret(c.Kind, secret) {
			return validation.Invalid("secret", "invalid channel secret")
		}
	case "telegram":
		if cfg.URL != "" || cfg.Host != "" || cfg.Port != 0 || cfg.Username != "" || cfg.From != "" || len(cfg.To) > 0 || !chatIDPattern.MatchString(cfg.ChatID) {
			return validation.Invalid("config", "requires a valid chat_id only")
		}
		if !tokenPattern.MatchString(secret) {
			return validation.Invalid("secret", "requires a bot token")
		}
	case "email":
		if cfg.URL != "" || cfg.ChatID != "" || !validHost(cfg.Host) || cfg.Port < 1 || cfg.Port > 65535 || !validAddress(cfg.From) || len(cfg.To) < 1 || len(cfg.To) > 20 || len(cfg.Username) > 100 || strings.ContainsAny(cfg.Username, "\r\n\x00") {
			return validation.Invalid("config", "requires SMTP host, port 1..65535, from and 1..20 recipients")
		}
		for _, to := range cfg.To {
			if !validAddress(to) {
				return validation.Invalid("config", "invalid recipient address")
			}
		}
		if strings.ContainsRune(secret, 0) {
			return validation.Invalid("secret", "cannot contain NUL")
		}
		if len(cfg.Username)+len(secret) > 360 {
			return validation.Invalid("secret", "SMTP credentials exceed command size limit")
		}
		if cfg.Username != "" && secret == "" {
			return validation.Invalid("secret", "password required with username")
		}
	default:
		return validation.Invalid("kind", "must be webhook, telegram, email, feishu, dingtalk, wecom, bark or ntfy")
	}
	if len(secret) > 4096 {
		return validation.Invalid("secret", "must be at most 4096 bytes")
	}
	return nil
}
func validAddress(s string) bool {
	if len(s) > 254 || strings.ContainsAny(s, "\r\n\x00") {
		return false
	}
	for _, b := range []byte(s) {
		if b < 32 || b >= 127 {
			return false
		}
	}
	a, e := mail.ParseAddress(s)
	return e == nil && a.Address == s
}

const channelColumns = "id,name,kind,enabled,config_json,secret_ciphertext,url_ciphertext,created_at,updated_at"

type scanner interface{ Scan(...any) error }

func scanChannel(row scanner) (Channel, []byte, []byte, error) {
	var c Channel
	var raw, created, updated string
	var cipher, urlCipher []byte
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.Enabled, &raw, &cipher, &urlCipher, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil, nil, ErrNotFound
	}
	if err != nil {
		return c, nil, nil, err
	}
	if err = json.Unmarshal([]byte(raw), &c.Config); err != nil {
		return c, nil, nil, err
	}
	c.SecretSet = len(cipher) > 0
	c.Config.URL = ""
	c.Config.URLSet = len(urlCipher) > 0
	c.CreatedAt, err = timekey.Parse(created)
	if err != nil {
		return c, nil, nil, err
	}
	c.UpdatedAt, err = timekey.Parse(updated)
	return c, cipher, urlCipher, err
}
func (s *Service) Get(ctx context.Context, id string) (Channel, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Channel{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	c, _, _, e := scanChannel(lease.QueryRowContext(ctx, "SELECT "+channelColumns+" FROM notification_channels WHERE id=?", id))
	return c, e
}
func (s *Service) List(ctx context.Context) ([]Channel, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	rows, e := lease.QueryContext(ctx, "SELECT "+channelColumns+" FROM notification_channels ORDER BY created_at,id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := make([]Channel, 0)
	for rows.Next() {
		c, _, _, e := scanChannel(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Service) Save(ctx context.Context, id string, p Patch) (Channel, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return Channel{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if s.box == nil {
		return Channel{}, ErrUnavailable
	}
	tx, e := lease.BeginWrite(ctx)
	if e != nil {
		return Channel{}, e
	}
	defer tx.Rollback()
	create := id == ""
	var c Channel
	var cipher, urlCipher []byte
	if create {
		var count int
		if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM notification_channels").Scan(&count); e != nil {
			return c, e
		}
		if count >= 20 {
			return c, ErrCapacity
		}
		if p.Name == nil || p.Kind == nil || p.Enabled == nil || len(p.Config) == 0 {
			return c, validation.Invalid("channel", "name, kind, enabled and config required")
		}
		c.ID, e = uuid.UUID(rand.Reader)
		if e != nil {
			return c, e
		}
		c.CreatedAt = s.opts.Now().UTC()
	} else {
		c, cipher, urlCipher, e = scanChannel(tx.QueryRowContext(ctx, "SELECT "+channelColumns+" FROM notification_channels WHERE id=?", id))
		if e != nil {
			return c, e
		}
	}
	if p.Name != nil {
		c.Name = *p.Name
	}
	if p.Enabled != nil {
		c.Enabled = *p.Enabled
	}
	if p.Kind != nil {
		if !create && *p.Kind != c.Kind {
			return c, validation.Invalid("kind", "cannot change kind")
		}
		c.Kind = *p.Kind
	}
	urlReplaced := false
	if len(p.Config) > 0 {
		// json.Unmarshal 会把非法字节静默替换成 U+FFFD，必须在解码前拒绝。
		if !utf8.Valid(p.Config) {
			return c, validation.Invalid("config", "must be valid UTF-8")
		}
		if bytes.Equal(bytes.TrimSpace(p.Config), []byte("null")) {
			return c, validation.Invalid("config", "cannot be null")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(p.Config, &fields) != nil || fields == nil {
			return c, validation.Invalid("config", "must be an object")
		}
		allowed := map[string]bool{}
		switch c.Kind {
		case "webhook", "feishu", "dingtalk", "wecom", "bark", "ntfy":
			allowed["url"] = true
			if c.Kind != "webhook" {
				allowed["template"] = true
			}
		case "telegram":
			allowed["chat_id"] = true
			allowed["template"] = true
		case "email":
			for _, key := range []string{"host", "port", "username", "from", "to", "template"} {
				allowed[key] = true
			}
		}
		for key, value := range fields {
			if !allowed[key] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return c, validation.Invalid("config", "unexpected or null configuration field")
			}
		}
		d := json.NewDecoder(bytes.NewReader(p.Config))
		d.DisallowUnknownFields()
		oldConfig := c.Config
		c.Config = Config{}
		if d.Decode(&c.Config) != nil {
			return c, validation.Invalid("config", "invalid configuration fields")
		}
		if UsesURL(c.Kind) {
			_, urlReplaced = fields["url"]
			if urlReplaced && c.Config.URL == "" {
				return c, validation.Invalid("config", "channel URL cannot be empty")
			}
			if !urlReplaced {
				c.Config.URLRedacted = oldConfig.URLRedacted
				c.Config.URLSet = oldConfig.URLSet
			}
		}
	}
	// 显式替换/清除绝不打开旧密文。URL 与签名密钥相互独立，
	// 因此可单独修复任一凭据。
	secret := ""
	if len(p.Secret) > 0 {
		if bytes.Equal(bytes.TrimSpace(p.Secret), []byte("null")) || json.Unmarshal(p.Secret, &secret) != nil {
			return c, validation.Invalid("secret", "must be a string; omit to retain or empty to clear")
		}
	} else if len(cipher) > 0 && c.Kind != "webhook" {
		plain, err := s.openSecret(c.ID, cipher)
		if err != nil {
			return c, ErrSecretUnreadable
		}
		secret = string(plain)
		clear(plain)
	}
	c.Config.Template = strings.TrimSpace(strings.ReplaceAll(c.Config.Template, "\r\n", "\n"))
	if e = s.validate(c, secret); e != nil {
		return c, e
	}
	if len(p.Secret) > 0 {
		cipher = nil
		if secret != "" {
			cipher, e = s.box.Seal(c.ID, []byte(secret))
			if e != nil {
				return c, e
			}
		}
	}
	if UsesURL(c.Kind) && (create || urlReplaced) {
		urlCipher, e = s.box.Seal(c.ID+":url", []byte(c.Config.URL))
		if e != nil {
			return c, e
		}
		u, _ := url.Parse(c.Config.URL) // 已在上方校验
		c.Config.URLRedacted = u.Scheme + "://" + u.Host + "/…"
		c.Config.URLSet = true
	}
	c.Config.URL = ""
	previousUpdated := c.UpdatedAt
	c.SecretSet = len(cipher) > 0
	c.UpdatedAt = s.opts.Now().UTC()
	if !create && !c.UpdatedAt.After(previousUpdated) {
		c.UpdatedAt = previousUpdated.Add(time.Nanosecond)
	}
	raw, _ := json.Marshal(c.Config)
	query := `INSERT INTO notification_channels(` + channelColumns + `) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,enabled=excluded.enabled,config_json=excluded.config_json,secret_ciphertext=excluded.secret_ciphertext,url_ciphertext=excluded.url_ciphertext,updated_at=excluded.updated_at`
	if tx.Dialect() == storage.MySQL {
		query = `INSERT INTO notification_channels(` + channelColumns + `) VALUES(?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE name=VALUES(name),enabled=VALUES(enabled),config_json=VALUES(config_json),secret_ciphertext=VALUES(secret_ciphertext),url_ciphertext=VALUES(url_ciphertext),updated_at=VALUES(updated_at)`
	}
	_, e = tx.ExecContext(ctx, query, c.ID, c.Name, c.Kind, c.Enabled, string(raw), cipher, urlCipher, timekey.Format(c.CreatedAt), timekey.Format(c.UpdatedAt))
	if e != nil {
		return c, e
	}
	action := "notification_channel.update"
	if create {
		action = "notification_channel.create"
	}
	if e = audit.Insert(ctx, tx, action, c.ID, c.UpdatedAt); e != nil {
		return c, e
	}
	return c, tx.Commit()
}
func (s *Service) Delete(ctx context.Context, id string) error {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, e := lease.BeginWrite(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	r, e := tx.ExecContext(ctx, "DELETE FROM notification_channels WHERE id=?", id)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return ErrNotFound
	}
	if e = audit.Insert(ctx, tx, "notification_channel.delete", id, s.opts.Now()); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) openSecret(id string, encrypted []byte) ([]byte, error) {
	if s.box == nil {
		return nil, ErrUnavailable
	}
	plain, e := s.box.Open(id, encrypted)
	if e != nil {
		return nil, ErrSecretUnreadable
	}
	return plain, nil
}
func (s *Service) load(ctx context.Context, id string) (Channel, string, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Channel{}, "", leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	c, encrypted, urlCipher, e := scanChannel(lease.QueryRowContext(ctx, "SELECT "+channelColumns+" FROM notification_channels WHERE id=?", id))
	if e != nil {
		return c, "", e
	}
	var secret string
	if len(encrypted) > 0 {
		plain, e := s.openSecret(id, encrypted)
		if e != nil {
			return c, "", e
		}
		secret = string(plain)
		clear(plain)
	}
	if UsesURL(c.Kind) {
		plain, e := s.openSecret(id+":url", urlCipher)
		if e != nil {
			return c, "", e
		}
		c.Config.URL = string(plain)
		clear(plain)
	}
	return c, secret, nil
}

// 对标量 patch 字段，拒绝显式 null，同时保留省略语义。
func (p *Patch) UnmarshalJSON(data []byte) error {
	type patch Patch
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		switch key {
		case "name", "kind", "enabled", "config", "secret":
		default:
			return validation.Invalid("channel", "unknown field")
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return validation.Invalid(key, "cannot be null")
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode((*patch)(p)); e != nil && e != io.EOF {
		return validation.Invalid("channel", "invalid field type")
	}
	return nil
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 || strings.Contains(host, "%") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, b := range []byte(label) {
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-') {
				return false
			}
		}
	}
	return true
}

var templateTokenPattern = regexp.MustCompile(`\{[a-z_]+\}`)
var templateTokens = []string{"{state}", "{rule}", "{node}", "{metric}", "{target}", "{severity}", "{value}", "{started_at}", "{resolved_at}", "{event_id}"}

func validTemplate(template string) bool {
	if !utf8.ValidString(template) || len(template) > 1000 {
		return false
	}
	for _, r := range template {
		if r != '\n' && unicode.IsControl(r) {
			return false
		}
	}
	for _, token := range templateTokenPattern.FindAllString(template, -1) {
		known := false
		for _, allowed := range templateTokens {
			if token == allowed {
				known = true
				break
			}
		}
		if !known {
			return false
		}
	}
	return true
}

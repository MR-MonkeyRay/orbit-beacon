package notify

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"orbitbeacon/internal/uuid"
)

type smtpError struct{ code int }

func (e *smtpError) Error() string { return "SMTP command rejected" }

var errSMTPProtocol = errors.New("invalid SMTP reply")

type smtpClient struct {
	conn      net.Conn
	reader    *bufio.Reader
	ctx       context.Context
	secure    bool
	startTLS  bool
	plainAuth bool
}

func (c *smtpClient) deadline() error {
	until := time.Now().Add(3 * time.Second)
	if deadline, ok := c.ctx.Deadline(); ok && deadline.Before(until) {
		until = deadline
	}
	return c.conn.SetDeadline(until)
}

// RFC 5321 的应答行（含 CRLF）最多 512 个八位字节。
// 同时限制单行长度和续行数量；绝不暴露原文。
func (c *smtpClient) reply(want ...int) ([]string, error) {
	if e := c.deadline(); e != nil {
		return nil, e
	}
	var lines []string
	code := 0
	for range 100 {
		raw, e := c.reader.ReadSlice('\n')
		if e != nil {
			if errors.Is(e, bufio.ErrBufferFull) {
				return nil, errSMTPProtocol
			}
			return nil, e
		}
		if len(raw) > 512 || len(raw) < 5 || string(raw[len(raw)-2:]) != "\r\n" {
			return nil, errSMTPProtocol
		}
		for _, b := range raw[:3] {
			if b < '0' || b > '9' {
				return nil, errSMTPProtocol
			}
		}
		n, _ := strconv.Atoi(string(raw[:3]))
		if n < 200 || n > 599 || code != 0 && n != code {
			return nil, errSMTPProtocol
		}
		code = n
		sep := byte(' ')
		if len(raw) > 5 {
			sep = raw[3]
		}
		if sep != ' ' && sep != '-' {
			return nil, errSMTPProtocol
		}
		text := ""
		if len(raw) > 5 {
			text = string(raw[4 : len(raw)-2])
		}
		lines = append(lines, text)
		if sep == ' ' {
			allowed := false
			for _, expected := range want {
				if n == expected {
					allowed = true
				}
			}
			if !allowed {
				return nil, &smtpError{code: n}
			}
			return lines, nil
		}
	}
	return nil, errSMTPProtocol
}
func (c *smtpClient) command(want int, command string) ([]string, error) {
	if strings.ContainsAny(command, "\r\n\x00") || len(command) > 510 {
		return nil, errSMTPProtocol
	}
	if e := c.deadline(); e != nil {
		return nil, e
	}
	if _, e := io.WriteString(c.conn, command+"\r\n"); e != nil {
		return nil, e
	}
	if strings.HasPrefix(command, "RCPT TO:") {
		return c.reply(250, 251, 252)
	}
	return c.reply(want)
}
func (c *smtpClient) ehlo() error {
	lines, e := c.command(250, "EHLO orbitbeacon")
	if e != nil {
		return e
	}
	c.startTLS = false
	c.plainAuth = false
	for _, line := range lines {
		fields := strings.Fields(strings.ToUpper(line))
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "STARTTLS" {
			c.startTLS = true
		}
		if fields[0] == "AUTH" {
			for _, method := range fields[1:] {
				if method == "PLAIN" {
					c.plainAuth = true
				}
			}
		}
	}
	return nil
}
func (c *smtpClient) auth(username, password string) error {
	if !c.secure {
		return errors.New("SMTP authentication requires TLS")
	}
	if !c.plainAuth {
		return &smtpError{code: 535}
	}
	raw := []byte("\x00" + username + "\x00" + password)
	defer clear(raw)
	encoded := base64.StdEncoding.EncodeToString(raw)
	_, e := c.command(235, "AUTH PLAIN "+encoded)
	return e
}
func (c *smtpClient) data(raw string) error {
	if _, e := c.command(354, "DATA"); e != nil {
		return e
	}
	if e := c.deadline(); e != nil {
		return e
	}
	// DotWriter 会把 LF 归一化为 CRLF、转义行首的点并写入结束符。
	// 裸 CR 也要归一化，确保任何输入都无法注入 DATA 结束符。
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n")
	writer := bufio.NewWriter(c.conn)
	dot := textproto.NewWriter(writer).DotWriter()
	if _, e := io.WriteString(dot, raw); e != nil {
		return e
	}
	if e := dot.Close(); e != nil {
		return e
	}
	if e := writer.Flush(); e != nil {
		return e
	}
	_, e := c.reply(250)
	return e
}
func (s *sender) email(ctx context.Context, c Channel, password string, p Payload) Result {
	cfg := c.Config
	conn, e := s.dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if e != nil {
		return networkFailure(e)
	}
	defer conn.Close()
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	client := &smtpClient{conn: conn, ctx: ctx}
	tlsCfg := s.tlsConfig.Clone()
	tlsCfg.ServerName = cfg.Host
	secure := func() error {
		if e := client.deadline(); e != nil {
			return e
		}
		tlsConn := tls.Client(client.conn, tlsCfg)
		if e := tlsConn.HandshakeContext(ctx); e != nil {
			return e
		}
		client.conn = tlsConn
		client.reader = bufio.NewReaderSize(tlsConn, 513)
		client.secure = true
		return nil
	}
	if cfg.Port == 465 {
		if e = secure(); e != nil {
			return smtpFailure(e)
		}
	} else {
		client.reader = bufio.NewReaderSize(conn, 513)
	}
	if _, e = client.reply(220); e != nil {
		return smtpFailure(e)
	}
	if e = client.ehlo(); e != nil {
		return smtpFailure(e)
	}
	if !client.secure {
		if !client.startTLS {
			return failure("starttls_required", false)
		}
		if _, e = client.command(220, "STARTTLS"); e != nil {
			return smtpFailure(e)
		}
		if client.reader.Buffered() != 0 {
			return failure("smtp_protocol", false)
		}
		if e = secure(); e != nil {
			return smtpFailure(e)
		}
		if e = client.ehlo(); e != nil {
			return smtpFailure(e)
		}
	}
	if cfg.Username != "" {
		if e = client.auth(cfg.Username, password); e != nil {
			return smtpFailure(e)
		}
	}
	if !validAddress(cfg.From) {
		return failure("invalid_destination", false)
	}
	if _, e = client.command(250, "MAIL FROM:<"+cfg.From+">"); e != nil {
		return smtpFailure(e)
	}
	for _, to := range cfg.To {
		if !validAddress(to) {
			return failure("invalid_destination", false)
		}
		if _, e = client.command(250, "RCPT TO:<"+to+">"); e != nil {
			return smtpFailure(e)
		}
	}
	raw, e := mailMessage(cfg, p, s.now())
	if e != nil {
		return failure("invalid_payload", false)
	}
	if e = client.data(raw); e != nil {
		return smtpFailure(e)
	}
	_, _ = client.command(221, "QUIT")
	return Result{OK: true}
}
func mailMessage(cfg Config, p Payload, now time.Time) (string, error) {
	if !validAddress(cfg.From) {
		return "", errSMTPProtocol
	}
	for _, to := range cfg.To {
		if !validAddress(to) {
			return "", errSMTPProtocol
		}
	}
	subject := "OrbitBeacon alert notification"
	id, e := uuid.UUID(rand.Reader)
	if e != nil {
		return "", e
	}
	body := base64.StdEncoding.EncodeToString([]byte(message(p)))
	var wrapped strings.Builder
	for len(body) > 0 {
		n := min(76, len(body))
		wrapped.WriteString(body[:n])
		wrapped.WriteString("\r\n")
		body = body[n:]
	}
	return fmt.Sprintf("Date: %s\r\nMessage-ID: <%s@orbitbeacon>\r\nFrom: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s", now.UTC().Format(time.RFC1123Z), id, cfg.From, strings.Join(cfg.To, ",\r\n "), subject, wrapped.String()), nil
}
func smtpFailure(e error) Result {
	var protocol *smtpError
	if errors.As(e, &protocol) {
		return failure("smtp_rejected", protocol.code >= 400 && protocol.code < 500)
	}
	if errors.Is(e, errSMTPProtocol) {
		return failure("smtp_protocol", false)
	}
	return networkFailure(e)
}

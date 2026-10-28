package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeSMTP(t *testing.T, mode string) (Config, *httptest.Server, <-chan string) {
	t.Helper()
	cert := httptest.NewTLSServer(nil)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { listener.Close(); cert.Close() })
	done := make(chan string, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(4 * time.Second))
		reader := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 fake SMTP\r\n")
		var captured strings.Builder
		secure := false
		for {
			line, e := reader.ReadString('\n')
			if e != nil {
				done <- captured.String()
				return
			}
			captured.WriteString(line)
			switch {
			case strings.HasPrefix(line, "EHLO "):
				if !secure && mode == "refuse" {
					fmt.Fprint(conn, "250-local\r\n250 AUTH PLAIN\r\n")
				} else {
					fmt.Fprint(conn, "250-local\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n")
				}
			case line == "STARTTLS\r\n":
				fmt.Fprint(conn, "220 begin TLS\r\n")
				tlsConn := tls.Server(conn, cert.TLS)
				if tlsConn.Handshake() != nil {
					done <- captured.String()
					return
				}
				conn = tlsConn
				reader = bufio.NewReader(conn)
				secure = true
			case strings.HasPrefix(line, "AUTH PLAIN "):
				if !secure {
					captured.WriteString("PLAINTEXT AUTH")
					done <- captured.String()
					return
				}
				if mode == "reject" {
					fmt.Fprint(conn, "535 secret password must not escape\r\n")
				} else {
					fmt.Fprint(conn, "235 authenticated\r\n")
				}
			case strings.HasPrefix(line, "MAIL FROM:") || strings.HasPrefix(line, "RCPT TO:"):
				fmt.Fprint(conn, "250 accepted\r\n")
			case line == "DATA\r\n":
				fmt.Fprint(conn, "354 body\r\n")
				for {
					line, e = reader.ReadString('\n')
					if e != nil {
						done <- captured.String()
						return
					}
					if line == ".\r\n" {
						break
					}
					captured.WriteString(line)
				}
				fmt.Fprint(conn, "250 queued\r\n")
			case line == "QUIT\r\n":
				fmt.Fprint(conn, "221 bye\r\n")
				done <- captured.String()
				return
			default:
				fmt.Fprint(conn, "500 invalid\r\n")
			}
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	var number int
	fmt.Sscan(port, &number)
	return Config{Host: "127.0.0.1", Port: number, Username: "smtp-user", From: "from@example.com", To: []string{"one@example.com", "two@example.com"}}, cert, done
}
func TestSMTPSTARTTLSFormationRefusalAndErrors(t *testing.T) {
	for _, mode := range []string{"success", "refuse", "reject"} {
		t.Run(mode, func(t *testing.T) {
			cfg, cert, done := fakeSMTP(t, mode)
			s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
			defer s.client.CloseIdleConnections()
			trustTestServer(s, cert)
			p := Payload{State: "firing"}
			p.Rule.Name = "热度\r\n.\r\nBcc: injected"
			result := s.Send(context.Background(), Channel{Kind: "email", Config: cfg}, "smtp-password", p)
			raw := <-done
			if strings.Contains(raw, "PLAINTEXT AUTH") {
				t.Fatal("plaintext credentials sent")
			}
			switch mode {
			case "success":
				if !result.OK {
					t.Fatalf("send=%+v", result)
				}
				auth := base64.StdEncoding.EncodeToString([]byte("\x00smtp-user\x00smtp-password"))
				if !strings.Contains(raw, "AUTH PLAIN "+auth) || !strings.Contains(raw, "RCPT TO:<two@example.com>") || !strings.Contains(raw, "Date: ") || !strings.Contains(raw, "Message-ID: <") || !strings.Contains(raw, "Content-Transfer-Encoding: base64") || strings.Contains(raw, "Bcc: injected") {
					t.Fatalf("invalid message")
				}
				parts := strings.SplitN(raw, "\r\n\r\n", 2)
				if len(parts) != 2 {
					t.Fatal("missing body")
				}
				body := strings.TrimSuffix(parts[1], "QUIT\r\n")
				decoded, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\r\n", ""))
				if e != nil || !strings.Contains(string(decoded), "热度") {
					t.Fatalf("body decoding: %v", e)
				}
			case "refuse":
				if result.OK || result.ErrorCode != "starttls_required" || strings.Contains(raw, "AUTH PLAIN ") {
					t.Fatalf("refused=%+v", result)
				}
			case "reject":
				if result.OK || result.retry || result.ErrorCode != "smtp_rejected" {
					t.Fatalf("reject=%+v", result)
				}
			}
		})
	}
}
func TestSMTPPlainAuthReplyBoundsAndDotStuffing(t *testing.T) {
	c := smtpClient{}
	if c.auth("user", "password") == nil {
		t.Fatal("plaintext AUTH accepted")
	}
	for _, raw := range []string{"250-first\r\n250 second\r\n", "250-first\r\n550 wrong-code\r\n", "250 bad\n", "xyz bad\r\n", "250 " + strings.Repeat("x", 513) + "\r\n", "250-continue\r\n"} {
		client, server := net.Pipe()
		go func() { defer server.Close(); io.WriteString(server, raw) }()
		c := smtpClient{conn: client, reader: bufio.NewReaderSize(client, 513), ctx: context.Background()}
		_, e := c.reply(250)
		client.Close()
		if (e == nil) != (raw == "250-first\r\n250 second\r\n") {
			t.Errorf("reply accepted=%v: %.30q", e == nil, raw)
		}
	}
	client, server := net.Pipe()
	done := make(chan string, 1)
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		line, _ := reader.ReadString('\n')
		if line != "DATA\r\n" {
			done <- "bad DATA"
			return
		}
		io.WriteString(server, "354 go\r\n")
		var raw strings.Builder
		for {
			line, _ = reader.ReadString('\n')
			raw.WriteString(line)
			if line == ".\r\n" {
				break
			}
		}
		io.WriteString(server, "250 accepted\r\n")
		done <- raw.String()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c = smtpClient{conn: client, reader: bufio.NewReader(client), ctx: ctx}
	if e := c.data("one\n.line\rbare\r\n.\n"); e != nil {
		t.Fatal(e)
	}
	client.Close()
	if raw := <-done; raw != "one\r\n..line\r\nbare\r\n..\r\n.\r\n" {
		t.Fatalf("dot stuffing=%q", raw)
	}
	for _, tt := range []struct {
		code  int
		retry bool
	}{{450, true}, {550, false}} {
		if r := smtpFailure(&smtpError{code: tt.code}); r.retry != tt.retry || r.ErrorCode != "smtp_rejected" {
			t.Fatal(r)
		}
	}
}
func TestSMTPHeadersRejectInjection(t *testing.T) {
	for _, cfg := range []Config{{From: "from@example.com\r\nBcc: attacker@example.com", To: []string{"to@example.com"}}, {From: "from@example.com", To: []string{"to@example.com\nBcc: attacker@example.com"}}} {
		if _, e := mailMessage(cfg, Payload{}, time.Now()); e == nil {
			t.Fatal("header injection accepted")
		}
	}
}

func TestSMTPAlternateSuccessAndReplyIOErrors(t *testing.T) {
	for _, code := range []int{250, 251, 252} {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			reader.ReadString('\n')
			fmt.Fprintf(server, "%d\r\n", code)
		}()
		c := smtpClient{conn: client, reader: bufio.NewReader(client), ctx: context.Background()}
		_, e := c.command(250, "RCPT TO:<recipient@example.com>")
		client.Close()
		if e != nil {
			t.Fatalf("valid RCPT %d refused: %v", code, e)
		}
	}
	client, server := net.Pipe()
	server.Close()
	c := smtpClient{conn: client, reader: bufio.NewReader(client), ctx: context.Background()}
	_, e := c.reply(220)
	client.Close()
	if !smtpFailure(e).retry {
		t.Fatal("network EOF is not retryable")
	}
}

func TestSMTPImplicitTLSAndUntrustedSTARTTLS(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		t.Run(fmt.Sprint(trusted), func(t *testing.T) {
			cfg, cert, done := fakeSMTP(t, "success")
			s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
			defer s.client.CloseIdleConnections()
			if trusted {
				trustTestServer(s, cert)
			}
			result := s.Send(context.Background(), Channel{Kind: "email", Config: cfg}, "password", Payload{})
			raw := <-done
			if result.OK != trusted {
				t.Fatalf("TLS trust result=%+v", result)
			}
			if !trusted && strings.Contains(raw, "AUTH PLAIN ") {
				t.Fatal("credentials leaked on failed TLS")
			}
		})
	}
	// Simulate port 465 over an ephemeral local listener while preserving the
	// production implicit-TLS branch and hostname certificate verification.
	cert := httptest.NewTLSServer(nil)
	defer cert.Close()
	listener, e := tls.Listen("tcp", "127.0.0.1:0", cert.TLS)
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		io.WriteString(conn, "220 implicit TLS\r\n")
		for {
			line, e := reader.ReadString('\n')
			if e != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO "):
				io.WriteString(conn, "250 implicit\r\n")
			case strings.HasPrefix(line, "MAIL FROM:") || strings.HasPrefix(line, "RCPT TO:"):
				io.WriteString(conn, "250 ok\r\n")
			case line == "DATA\r\n":
				io.WriteString(conn, "354 go\r\n")
				for {
					line, e = reader.ReadString('\n')
					if e != nil {
						return
					}
					if line == ".\r\n" {
						break
					}
				}
				io.WriteString(conn, "250 accepted\r\n")
			case line == "QUIT\r\n":
				io.WriteString(conn, "221 bye\r\n")
				return
			default:
				t.Errorf("unexpected implicit command %q", line)
				return
			}
		}
	}()
	s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	trustTestServer(s, cert)
	d := net.Dialer{}
	s.dialer.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "127.0.0.1:465" {
			t.Error("implicit port lost")
		}
		return d.DialContext(ctx, network, listener.Addr().String())
	}
	result := s.Send(context.Background(), Channel{Kind: "email", Config: Config{Host: "127.0.0.1", Port: 465, From: "from@example.com", To: []string{"to@example.com"}}}, "", Payload{})
	if !result.OK {
		t.Fatalf("implicit TLS=%+v", result)
	}
	<-done
}

func TestSMTPCancellationInterruptsGreeting(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		io.Copy(io.Discard, conn)
	}()
	_, rawPort, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	fmt.Sscan(rawPort, &port)
	s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := s.Send(ctx, Channel{Kind: "email", Config: Config{Host: "127.0.0.1", Port: port, From: "from@example.com", To: []string{"to@example.com"}}}, "", Payload{})
	if result.OK || !result.retry || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("cancellation %+v elapsed %v", result, time.Since(start))
	}
	<-done
}

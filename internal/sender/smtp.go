package sender

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"emailblast/internal/model"
	"emailblast/internal/render"
)

// SMTPSender delivers through any SMTP server — a mail server you run on your
// own VPS (Postfix/Exim), or a relay (SES SMTP endpoint, Mailgun, etc.). No
// external Go dependency; uses the stdlib.
//
// VPS reality check: a fresh VPS IP has zero reputation. Sending 1M cold from it
// = instant spam-folder / blacklist. To use a VPS seriously you must: set up
// SPF + DKIM + DMARC DNS records, reverse-DNS (PTR) matching your HELO name,
// warm the IP up over days, and handle bounces. For 1M, a reputable ESP is
// usually the pragmatic choice; a self-run VPS relay makes sense when you own
// deliverability end-to-end. This backend lets you do either.
//
// A new smtp.Client is opened per Send for simplicity and isolation. For max
// throughput swap in a pooled/persistent-connection dialer, but per-connection
// send keeps one slow/broken connection from poisoning others, and the worker
// pool already provides the concurrency.
type SMTPSender struct {
	host string // "mail.example.com"
	port string // "587" (STARTTLS) or "25"
	from string // envelope + header From
	auth smtp.Auth
	tls  bool // use STARTTLS
}

// NewSMTP builds an SMTP backend. Pass user/pass "" for an unauthenticated
// local relay (common when the app runs ON the same VPS as Postfix on :25).
func NewSMTP(host, port, from, user, pass string, useTLS bool) *SMTPSender {
	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}
	return &SMTPSender{host: host, port: port, from: from, auth: auth, tls: useTLS}
}

func (s *SMTPSender) Name() string { return "smtp" }

func (s *SMTPSender) Send(ctx context.Context, u model.User, msg render.Message, key string) error {
	addr := net.JoinHostPort(s.host, s.port)

	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: dial %s: %v", ErrRetryable, addr, err) // network -> retryable
	}
	defer conn.Close()

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return fmt.Errorf("%w: smtp client: %v", ErrRetryable, err)
	}
	defer c.Close()

	if s.tls {
		if err := c.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
			return fmt.Errorf("%w: starttls: %v", ErrRetryable, err)
		}
	}
	if s.auth != nil {
		if err := c.Auth(s.auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err) // permanent
		}
	}
	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("%w: MAIL FROM: %v", ErrRetryable, err)
	}
	if err := c.Rcpt(u.Email); err != nil {
		// 5xx here usually means bad recipient -> permanent; 4xx -> retryable.
		if strings.HasPrefix(strings.TrimSpace(err.Error()), "4") {
			return fmt.Errorf("%w: RCPT: %v", ErrRetryable, err)
		}
		return fmt.Errorf("RCPT %s: %w", u.Email, err)
	}

	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("%w: DATA: %v", ErrRetryable, err)
	}
	if _, err := wc.Write(buildMIME(s.from, u.Email, key, msg)); err != nil {
		return fmt.Errorf("%w: write body: %v", ErrRetryable, err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("%w: close data: %v", ErrRetryable, err)
	}
	return c.Quit()
}

// buildMIME assembles a minimal RFC 5322 HTML message. Message-ID carries the
// idempotency key so a resend after crash is detectable downstream.
func buildMIME(from, to, key string, msg render.Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	fmt.Fprintf(&b, "Message-ID: <%s@emailblast>\r\n", key)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.Body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

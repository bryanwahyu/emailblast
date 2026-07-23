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
// Connection pooling: a TCP + STARTTLS + AUTH handshake per message is the
// dominant cost on the SMTP path. This sender keeps a bounded pool of persistent
// smtp.Client connections and issues RSET between messages to reuse them, so one
// handshake is amortized over many sends. A connection that errors is discarded
// (never returned to the pool) so one broken link can't poison the others.
type SMTPSender struct {
	host string // "mail.example.com"
	port string // "587" (STARTTLS) or "25"
	from string // envelope + header From
	auth  smtp.Auth
	tls   bool // use STARTTLS
	unsub Unsubscribe

	pool chan *smtp.Client // idle, ready-to-reuse connections
}

// NewSMTP builds an SMTP backend with a connection pool of the given size.
// poolSize <= 0 defaults to 1. Pass user/pass "" for an unauthenticated local
// relay (common when the app runs ON the same VPS as Postfix on :25).
//
// Size the pool to roughly the number of concurrent workers hitting SMTP; extra
// connections beyond that just sit idle.
func NewSMTP(host, port, from, user, pass string, useTLS bool, poolSize int, unsub Unsubscribe) *SMTPSender {
	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}
	if poolSize <= 0 {
		poolSize = 1
	}
	return &SMTPSender{
		host: host, port: port, from: from, auth: auth, tls: useTLS, unsub: unsub,
		pool: make(chan *smtp.Client, poolSize),
	}
}

func (s *SMTPSender) Name() string { return "smtp" }

// getConn returns a pooled connection if one is idle, otherwise dials a fresh
// one and completes the STARTTLS + AUTH handshake.
func (s *SMTPSender) getConn(ctx context.Context) (*smtp.Client, error) {
	select {
	case c := <-s.pool:
		return c, nil // reuse: handshake already done
	default:
	}
	return s.dial(ctx)
}

// putConn returns a healthy connection to the pool, or closes it if the pool is
// full. The caller must have already RSET the transaction.
func (s *SMTPSender) putConn(c *smtp.Client) {
	select {
	case s.pool <- c:
	default:
		c.Close() // pool full: drop the extra
	}
}

func (s *SMTPSender) dial(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.host, s.port)
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %v", ErrRetryable, addr, err)
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: smtp client: %v", ErrRetryable, err)
	}
	if s.tls {
		if err := c.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
			c.Close()
			return nil, fmt.Errorf("%w: starttls: %v", ErrRetryable, err)
		}
	}
	if s.auth != nil {
		if err := c.Auth(s.auth); err != nil {
			c.Close()
			return nil, fmt.Errorf("smtp auth: %w", err) // permanent
		}
	}
	return c, nil
}

func (s *SMTPSender) Send(ctx context.Context, u model.User, msg render.Message, key string) error {
	c, err := s.getConn(ctx)
	if err != nil {
		return err // already wrapped (retryable dial / permanent auth)
	}

	// On any protocol error the connection state is undefined -> close & drop,
	// never return it to the pool. errRetryable classification below drives the
	// worker's retry loop, which will dial a fresh connection.
	if err := s.deliver(c, u, msg, key); err != nil {
		c.Close()
		return err
	}

	// Reset transaction so the next message can reuse this connection.
	if err := c.Reset(); err != nil {
		c.Close()
		return nil // send already succeeded; just don't reuse this conn
	}
	s.putConn(c)
	return nil
}

// deliver runs one MAIL/RCPT/DATA transaction on an established connection.
func (s *SMTPSender) deliver(c *smtp.Client, u model.User, msg render.Message, key string) error {
	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("%w: MAIL FROM: %v", ErrRetryable, err)
	}
	if err := c.Rcpt(u.Email); err != nil {
		// 4xx -> transient (greylisting, rate) -> retryable; 5xx -> bad address.
		if strings.HasPrefix(strings.TrimSpace(err.Error()), "4") {
			return fmt.Errorf("%w: RCPT: %v", ErrRetryable, err)
		}
		return fmt.Errorf("RCPT %s: %w", u.Email, err)
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("%w: DATA: %v", ErrRetryable, err)
	}
	if _, err := wc.Write(buildMIME(s.from, u.Email, key, msg, s.unsub)); err != nil {
		wc.Close()
		return fmt.Errorf("%w: write body: %v", ErrRetryable, err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("%w: close data: %v", ErrRetryable, err)
	}
	return nil
}

// buildMIME assembles a minimal RFC 5322 HTML message. Message-ID carries the
// idempotency key so a resend after crash is detectable downstream. When
// unsubscribe is configured, List-Unsubscribe (+ List-Unsubscribe-Post for
// one-click, RFC 8058) headers are added — required by Gmail/Yahoo bulk rules.
func buildMIME(from, to, key string, msg render.Message, unsub Unsubscribe) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	fmt.Fprintf(&b, "Message-ID: <%s@emailblast>\r\n", key)
	if v := unsub.Header(to); v != "" {
		fmt.Fprintf(&b, "List-Unsubscribe: %s\r\n", v)
		if unsub.OneClick() {
			b.WriteString("List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n")
		}
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.Body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

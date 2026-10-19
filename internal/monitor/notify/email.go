package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Email sends a plain-text message over SMTP.
//
// STARTTLS is used whenever the server offers it, and authentication is only
// attempted after it: net/smtp's PlainAuth refuses to send a password over an
// unencrypted connection to anything but localhost, which is the behaviour
// wanted, so a misconfigured relay fails loudly rather than leaking.
type Email struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	To       []string
	// TLSConfig overrides the STARTTLS configuration (tests).
	TLSConfig *tls.Config
}

// Notify implements Notifier.
func (e *Email) Notify(ctx context.Context, n Notification) error {
	msg, err := EmailMessage(e.From, e.To, n)
	if err != nil {
		return &Permanent{err}
	}
	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	// net/smtp has no context, so the deadline is put on the connection: an
	// SMTP server that accepts and then stalls must not hold the worker.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if ok, _ := c.Extension("STARTTLS"); ok {
		cfg := e.TLSConfig
		if cfg == nil {
			cfg = &tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12}
		}
		if err := c.StartTLS(cfg); err != nil {
			return err
		}
	}
	if e.User != "" {
		if err := c.Auth(smtp.PlainAuth("", e.User, e.Password, e.Host)); err != nil {
			return &Permanent{fmt.Errorf("smtp auth: %w", err)}
		}
	}
	if err := c.Mail(e.From); err != nil {
		return err
	}
	for _, to := range e.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// EmailMessage builds the RFC 5322 message: UTF-8 text, quoted-printable, with
// the subject encoded so a group key with non-ASCII in it survives.
//
// Header injection is the thing to get right. Every header value that comes
// from outside — a monitor name, a group value — goes through
// mime.QEncoding, which never emits a raw CR or LF, so a monitor named
// "x\r\nBcc: someone" is a strange subject and not a second recipient.
func EmailMessage(from string, to []string, n Notification) ([]byte, error) {
	if _, err := mail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("from %q: %w", from, err)
	}
	if len(to) == 0 {
		return nil, errors.New("no recipients")
	}
	for _, a := range to {
		if _, err := mail.ParseAddress(a); err != nil {
			return nil, fmt.Errorf("to %q: %w", a, err)
		}
	}
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", from)
	h("To", strings.Join(to, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", n.Title()))
	h("Date", n.Ts.UTC().Format(time.RFC1123Z))
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	value := "no data"
	if n.Value != nil {
		value = strconv.FormatFloat(*n.Value, 'g', -1, 64)
	}
	body := fmt.Sprintf("%s\n\nStatus: %s -> %s\nValue: %s (threshold %g)\nGroup: %s\n",
		n.Message, n.PrevStatus, n.Status, value, n.Threshold, n.GroupKey)
	if n.Link != "" {
		body += "Link: " + n.Link + "\n"
	}
	if _, err := qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n"))); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

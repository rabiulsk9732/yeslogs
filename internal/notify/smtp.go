// Package notify delivers operational alerts (currently SMTP email). It has no
// dependencies beyond the standard library so it stays trivially testable and
// embeddable in the single natlog binary.
package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTP is an SMTP relay configuration capable of sending a plain-text message.
type SMTP struct {
	Host string
	Port int
	User string // empty = no AUTH
	Pass string
	TLS  string // "starttls" (587) | "tls" (implicit, 465) | "none" (25)
	From string
	// Org signs the HTML view when a body carries no "— …" sign-off of its own.
	// Empty falls back to defaultOrg.
	Org string
}

// Send delivers one plain-text message to the recipients. It honours the ctx
// deadline for the dial; the SMTP conversation itself uses the connection
// deadline derived from ctx.
func (m SMTP) Send(ctx context.Context, to []string, subject, body string) error {
	if strings.TrimSpace(m.Host) == "" {
		return fmt.Errorf("smtp host not configured")
	}
	if len(to) == 0 {
		return fmt.Errorf("no recipients")
	}
	port := m.Port
	if port == 0 {
		port = 587
	}
	from := strings.TrimSpace(m.From)
	if from == "" {
		from = m.User
	}
	if from == "" {
		return fmt.Errorf("no From address (set From or SMTP user)")
	}
	addr := net.JoinHostPort(m.Host, strconv.Itoa(port))
	mode := strings.ToLower(strings.TrimSpace(m.TLS))
	if mode == "" {
		mode = "starttls"
	}
	tlsConf := &tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12}

	d := net.Dialer{Timeout: 15 * time.Second}
	if dl, ok := ctx.Deadline(); ok {
		d.Deadline = dl
	}

	var conn net.Conn
	var err error
	if mode == "tls" { // implicit TLS from the first byte (465)
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tlsConf)
	} else { // starttls / none — start plaintext, optionally upgrade
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		_ = conn.Close() // NewClient does not own the conn on failure
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		// best-effort overall deadline on the underlying conn
		type deadliner interface{ SetDeadline(time.Time) error }
		if dc, ok := any(c).(deadliner); ok {
			_ = dc.SetDeadline(dl)
		}
	}

	if mode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConf); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		} else {
			return fmt.Errorf("server does not offer STARTTLS (try TLS mode 'tls' or 'none')")
		}
	}
	if m.User != "" {
		if mode == "none" {
			// Never transmit credentials over an unencrypted connection.
			return fmt.Errorf("refusing SMTP AUTH over an unencrypted connection: set TLS to 'starttls' or 'tls'")
		}
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Pass, m.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT %s: %w", rcpt, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := wc.Write(buildMessage(from, to, subject, body, m.Org)); err != nil {
		_ = wc.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp body close: %w", err)
	}
	return c.Quit()
}

// mimeBoundary is fixed rather than random: every part of this message is
// generated here, so there is nothing that could contain it, and a deterministic
// boundary keeps buildMessage byte-for-byte testable.
const mimeBoundary = "natlog-alert-boundary-1f4a9c2e"

func buildMessage(from string, to []string, subject, body, org string) []byte {
	// Strip CR/LF from header values to prevent header injection (e.g. a device
	// name carrying "\r\nBcc:" reaching the Subject). Body CRLF is legitimate.
	hdr := strings.NewReplacer("\r", " ", "\n", " ")
	safeTo := make([]string, len(to))
	for i, t := range to {
		safeTo[i] = hdr.Replace(t)
	}
	var b strings.Builder
	b.WriteString("From: " + hdr.Replace(from) + "\r\n")
	b.WriteString("To: " + strings.Join(safeTo, ", ") + "\r\n")
	b.WriteString("Subject: " + hdr.Replace(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	// multipart/alternative, plain text FIRST. Order is significant: the last
	// part a client can display is the one it shows, so HTML-capable readers get
	// the formatted view while text-only readers and pagers still get the whole
	// report rather than markup.
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + mimeBoundary + "\"\r\n")
	b.WriteString("\r\n")

	b.WriteString("--" + mimeBoundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	b.WriteString("\r\n")

	b.WriteString("--" + mimeBoundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	// The subject is sanitised for the HTML title too. A CRLF smuggled through a
	// device name cannot inject a header from inside a MIME part, but it would
	// still break the rendered title across two lines and put an attacker's text
	// where a heading belongs.
	b.WriteString(strings.ReplaceAll(renderHTML(org, hdr.Replace(subject), body), "\n", "\r\n"))
	b.WriteString("\r\n")

	b.WriteString("--" + mimeBoundary + "--\r\n")
	return []byte(b.String())
}

// SplitRecipients parses a comma/newline/space-separated recipient list into a
// deduped slice of trimmed addresses.
func SplitRecipients(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';' || r == ' ' || r == '\t'
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// Package smtpx sends the one kind of mail mailkite ever sends: a short plain-text
// unsubscribe request to the address of a List-Unsubscribe mailto, after the user
// confirmed it. A connection lives for one message and is closed before Send
// returns; it starts no goroutines and keeps no timers.
package smtpx

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/kad-xyz/mailkite/internal/config"
)

const (
	dialTimeout = 15 * time.Second
	opTimeout   = 2 * time.Minute // whole session, when ctx has no earlier deadline
)

// Options tune SendWith for tests.
type Options struct {
	// Insecure talks plain TCP without TLS (and allows PLAIN auth over it). Tests only.
	Insecure bool
	roots    *x509.CertPool // tests: trust this CA instead of the system roots
}

// headerValue drops CR, LF and other control characters, so a value can never
// start a new header line.
func headerValue(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return "localhost"
}

func messageID(from string) string {
	var b [12]byte
	rand.Read(b[:]) // never fails (crypto/rand panics instead)
	return "<" + hex.EncodeToString(b[:]) + "." + strconv.FormatInt(time.Now().Unix(), 36) + "@" + headerValue(domainOf(from)) + ">"
}

// Build makes a text/plain UTF-8 message (quoted-printable) with From, To,
// Subject, Date and Message-ID. Header values lose any control characters.
func Build(from, to, subject, body string) []byte {
	from, to = headerValue(from), headerValue(to)
	var b bytes.Buffer
	put := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	put("From", from)
	put("To", to)
	put("Subject", mime.QEncoding.Encode("utf-8", headerValue(subject)))
	put("Date", time.Now().Format(time.RFC1123Z))
	put("Message-ID", messageID(from))
	put("MIME-Version", "1.0")
	put("Content-Type", "text/plain; charset=utf-8")
	put("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ToValidUTF8(body, "�")))
	qp.Close()
	if !bytes.HasSuffix(b.Bytes(), []byte("\r\n")) {
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// ValidAddress accepts a bare local@domain address without spaces, brackets or
// control characters.
func ValidAddress(s string) bool {
	l, d, ok := strings.Cut(s, "@")
	if !ok || l == "" || d == "" || strings.Contains(d, "@") || len(s) > 254 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune("<>,;\"()[]\\", r)
	})
}

// deadlineConn caps every deadline go-smtp sets at the session's deadline.
type deadlineConn struct {
	net.Conn
	max time.Time
}

func (c deadlineConn) clamp(t time.Time) time.Time {
	if t.IsZero() || t.After(c.max) {
		return c.max
	}
	return t
}

func (c deadlineConn) SetDeadline(t time.Time) error      { return c.Conn.SetDeadline(c.clamp(t)) }
func (c deadlineConn) SetReadDeadline(t time.Time) error  { return c.Conn.SetReadDeadline(c.clamp(t)) }
func (c deadlineConn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(c.clamp(t)) }

// Send delivers raw to the recipients through the account's SMTP server, as
// a.Email with PLAIN auth: implicit TLS ("tls", port 465 by default) or STARTTLS
// ("starttls", port 587 by default). Auth never happens without TLS.
func Send(ctx context.Context, a config.Account, password, from string, to []string, raw []byte) error {
	return SendWith(ctx, a, password, from, to, raw, Options{})
}

// SendWith is Send with options.
func SendWith(ctx context.Context, a config.Account, password, from string, to []string, raw []byte, o Options) error {
	if a.SMTPHost == "" || a.Email == "" {
		return errors.New("smtp: account has no SMTP host or email")
	}
	if !ValidAddress(from) {
		return errors.New("smtp: invalid sender address")
	}
	if len(to) == 0 {
		return errors.New("smtp: no recipients")
	}
	for _, t := range to {
		if !ValidAddress(t) {
			return errors.New("smtp: invalid recipient address")
		}
	}
	mode := strings.ToLower(a.SMTPTLS)
	if mode == "" {
		mode = "starttls"
		if a.SMTPPort == 465 {
			mode = "tls"
		}
	}
	if mode != "tls" && mode != "starttls" {
		return fmt.Errorf("smtp: unknown TLS mode %q (want tls|starttls)", a.SMTPTLS)
	}
	port := a.SMTPPort
	if port == 0 {
		port = 587
		if mode == "tls" {
			port = 465
		}
	}
	addr := net.JoinHostPort(a.SMTPHost, strconv.Itoa(port))
	max := time.Now().Add(opTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(max) {
		max = d
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	tcfg := &tls.Config{ServerName: a.SMTPHost, MinVersion: tls.VersionTLS12, RootCAs: o.roots}
	nd := &net.Dialer{Timeout: dialTimeout}
	nc, err := nd.DialContext(dctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: connect %s: %w", addr, err)
	}
	// Deadlines are capped below the TLS layer, so go-smtp still sees a *tls.Conn
	// (it detects encryption by that type).
	conn := deadlineConn{nc, max}
	conn.SetDeadline(time.Time{})
	var c *smtp.Client
	switch {
	case o.Insecure:
		c = smtp.NewClient(conn)
	case mode == "tls":
		tc := tls.Client(conn, tcfg)
		if err := tc.HandshakeContext(dctx); err != nil {
			nc.Close()
			return fmt.Errorf("smtp: tls %s: %w", addr, err)
		}
		c = smtp.NewClient(tc)
	default:
		if c, err = smtp.NewClientStartTLS(conn, tcfg); err != nil {
			nc.Close()
			return fmt.Errorf("smtp: starttls %s: %w", addr, err)
		}
	}
	defer c.Close()
	if !o.Insecure {
		if _, ok := c.TLSConnectionState(); !ok {
			return errors.New("smtp: connection is not encrypted; refusing to log in")
		}
	}
	if err := c.Auth(sasl.NewPlainClient("", a.Email, password)); err != nil {
		return fmt.Errorf("smtp: login: %w", err)
	}
	if err := c.Mail(from, nil); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	for _, t := range to {
		if err := c.Rcpt(t, nil); err != nil {
			return fmt.Errorf("smtp: RCPT TO: %w", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		w.Close()
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	_ = c.Quit()
	return nil
}

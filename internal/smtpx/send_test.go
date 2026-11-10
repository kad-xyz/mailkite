package smtpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/kad-xyz/mailkite/internal/config"
)

// Received is what the test server got in one session.
type Received struct {
	User, Pass string
	From       string
	To         []string
	Data       []byte
}

// TestServer is a go-smtp server on 127.0.0.1:0 (plain TCP, PLAIN auth allowed).
type TestServer struct {
	Host string
	Port int
	mu   sync.Mutex
	got  []Received
}

func (s *TestServer) Got() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.got...)
}

type session struct {
	s *TestServer
	r Received
}

func (x *session) AuthMechanisms() []string { return []string{sasl.Plain} }
func (x *session) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if pass != "secret" {
			return errors.New("bad password")
		}
		x.r.User, x.r.Pass = user, pass
		return nil
	}), nil
}
func (x *session) Mail(from string, _ *smtp.MailOptions) error { x.r.From = from; return nil }
func (x *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	x.r.To = append(x.r.To, to)
	return nil
}
func (x *session) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	x.r.Data = b
	x.s.mu.Lock()
	x.s.got = append(x.s.got, x.r)
	x.s.mu.Unlock()
	return err
}
func (x *session) Reset()        {}
func (x *session) Logout() error { return nil }

func startServer(t *testing.T) *TestServer {
	t.Helper()
	ts := &TestServer{}
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return &session{s: ts}, nil }))
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	host, port, _ := net.SplitHostPort(l.Addr().String())
	ts.Host = host
	ts.Port, _ = strconv.Atoi(port)
	return ts
}

func TestSendBuildHeaders(t *testing.T) {
	raw := Build("me@example.com", "unsub@list.example", "Отписка\r\nBcc: evil@x.example", "please\nunsubscribe")
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	h := m.Header
	if h.Get("From") != "me@example.com" || h.Get("To") != "unsub@list.example" {
		t.Fatalf("from/to %q %q", h.Get("From"), h.Get("To"))
	}
	if h.Get("Bcc") != "" || h.Get("Cc") != "" {
		t.Fatal("header injection through subject")
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(h.Get("Subject"))
	if err != nil || !strings.HasPrefix(subj, "Отписка") || strings.ContainsAny(subj, "\r\n") {
		t.Fatalf("subject %q %v", subj, err)
	}
	if _, err := mail.ParseDate(h.Get("Date")); err != nil {
		t.Fatalf("date %q: %v", h.Get("Date"), err)
	}
	if id := h.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Fatalf("message-id %q", id)
	}
	if ct := h.Get("Content-Type"); !strings.Contains(ct, "text/plain") || !strings.Contains(strings.ToLower(ct), "charset=utf-8") {
		t.Fatalf("content-type %q", ct)
	}
	if h.Get("MIME-Version") != "1.0" {
		t.Fatal("no MIME-Version")
	}
	if !bytes.Contains(raw, []byte("\r\n\r\n")) || bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("lines must end with CRLF")
	}
	if Build("me@example.com", "a@b.example", "x", "y") == nil {
		t.Fatal("empty build")
	}
	other := Build("me@example.com", "a@b.example", "x", "y")
	m2, _ := mail.ReadMessage(bytes.NewReader(other))
	if m2.Header.Get("Message-ID") == h.Get("Message-ID") {
		t.Fatal("Message-ID must be unique")
	}
}

func TestSendWithInsecure(t *testing.T) {
	ts := startServer(t)
	a := config.Account{Email: "me@example.com", SMTPHost: ts.Host, SMTPPort: ts.Port, SMTPTLS: "starttls"}
	raw := Build("me@example.com", "unsub@list.example", "unsubscribe", "unsubscribe")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := SendWith(ctx, a, "secret", "me@example.com", []string{"unsub@list.example"}, raw, Options{Insecure: true}); err != nil {
		t.Fatal(err)
	}
	got := ts.Got()
	if len(got) != 1 {
		t.Fatalf("got %d messages", len(got))
	}
	r := got[0]
	if r.User != "me@example.com" || r.From != "me@example.com" || len(r.To) != 1 || r.To[0] != "unsub@list.example" {
		t.Fatalf("%+v", r)
	}
	if !bytes.Contains(r.Data, []byte("Subject: unsubscribe")) {
		t.Fatalf("data %q", r.Data)
	}
}

func TestSendRefusesWithoutTLS(t *testing.T) {
	ts := startServer(t) // no STARTTLS offered
	a := config.Account{Email: "me@example.com", SMTPHost: ts.Host, SMTPPort: ts.Port, SMTPTLS: "starttls"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw := Build("me@example.com", "u@list.example", "unsubscribe", "unsubscribe")
	if err := Send(ctx, a, "secret", "me@example.com", []string{"u@list.example"}, raw); err == nil {
		t.Fatal("STARTTLS missing must fail")
	}
	if len(ts.Got()) != 0 {
		t.Fatal("nothing may be sent without TLS")
	}
	for _, bad := range []struct{ from, to string }{{"me@example.com", "x@y\r\nRCPT TO:<z@w>"}, {"", "u@list.example"}} {
		if err := SendWith(ctx, a, "secret", bad.from, []string{bad.to}, raw, Options{Insecure: true}); err == nil {
			t.Fatalf("bad address %q -> %q accepted", bad.from, bad.to)
		}
	}
	if err := SendWith(ctx, config.Account{Email: "me@example.com"}, "secret", "me@example.com", []string{"u@list.example"}, raw, Options{Insecure: true}); err == nil {
		t.Fatal("no SMTP host must fail")
	}
}

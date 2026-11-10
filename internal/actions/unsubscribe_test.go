package actions

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/message"
	"github.com/kad-xyz/mailkite/internal/smtpx"
)

func parsed(t *testing.T, headers, body string) *message.Message {
	t.Helper()
	m, err := message.Parse(rawMsg("Shop <news@shop.example>", "<p@shop.example>", "Скидки", headers, body))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPickMethod(t *testing.T) {
	cases := []struct {
		name, headers, body string
		method, target      string
	}{
		{"oneclick", "List-Unsubscribe: <mailto:u@shop.example>, <https://shop.example/u?id=1>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n",
			"hi", "oneclick", "https://shop.example/u?id=1"},
		{"mailto", "List-Unsubscribe: <https://shop.example/u?id=1>, <mailto:u@shop.example?subject=stop>\r\n",
			"hi", "mailto", "mailto:u@shop.example?subject=stop"},
		{"manual", "", "To unsubscribe click https://shop.example/unsubscribe?id=7 now",
			"manual", "https://shop.example/unsubscribe?id=7"},
		{"none", "", "Hello, no links here", "none", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pickMethod(parsed(t, c.headers, c.body), false)
			if got.Method != c.method || got.URL != c.target {
				t.Fatalf("got %+v, want %s %s", got, c.method, c.target)
			}
		})
	}
}

func TestOneClickURLGuard(t *testing.T) {
	for _, u := range []string{
		"http://shop.example/u",
		"https://user:pw@shop.example/u",
		"https://127.0.0.1/u",
		"https://10.0.0.5/u",
		"https://192.168.1.1/u",
		"https://[::1]/u",
		"https://169.254.169.254/latest",
		"https:///nohost",
		"ftp://shop.example/u",
		"https://100.64.0.1/u",
		"https://[fc00::1]/u",
		"https://[fe80::1]/u",
		"https://[::ffff:127.0.0.1]/u",
		"https://0.0.0.0/u",
		"https://192.0.0.8/u",
		"https://198.18.0.1/u",
		"https://198.19.255.1/u",
		"https://240.0.0.1/u",
		"https://[64:ff9b::7f00:1]/u",
		"https://[64:ff9b::808:808]/u",
		"https://[2002:7f00:1::1]/u",
		"https://[2002:808:808::1]/u",
	} {
		if err := checkOneClickURL(u, false); err == nil {
			t.Errorf("%s must be refused", u)
		}
	}
	for _, ok := range []string{"https://shop.example/u?a=1", "https://8.8.8.8/u", "https://[2606:4700::1111]/u"} {
		if err := checkOneClickURL(ok, false); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
}

// TestOneClickDialGuard skips the URL check and shows the dialer itself refuses
// a loopback peer when no test hook is set.
func TestOneClickDialGuard(t *testing.T) {
	hit := false
	s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer s.Close()
	e := &Engine{}
	c, tr := e.unsubClient()
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/u", strings.NewReader(oneClickBody))
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("loopback peer must be refused at dial")
	}
	if !strings.Contains(err.Error(), "refusing a private or local address") || hit {
		t.Fatalf("err %v hit %v", err, hit)
	}
}

// oneClickServer is an httptest TLS server recording POSTs per path.
type oneClickServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits []*http.Request
	body []string
}

func (s *oneClickServer) requests() ([]*http.Request, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.hits...), append([]string(nil), s.body...)
}

func newOneClick(t *testing.T, v *env) *oneClickServer {
	t.Helper()
	s := &oneClickServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.hits = append(s.hits, r)
		s.body = append(s.body, string(b))
		s.mu.Unlock()
		switch r.URL.Path {
		case "/fail":
			w.WriteHeader(http.StatusInternalServerError)
		case "/to-http":
			http.Redirect(w, r, "http://"+r.Host+"/ok", http.StatusTemporaryRedirect)
		case "/see-other":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(s.Close)
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	v.e.testNet = &testNet{roots: pool}
	v.e.Version = "test"
	return s
}

func oneClickHeaders(u string) *string {
	h := "List-Id: <news.shop.example>\r\nList-Unsubscribe: <" + u + ">\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\nPrecedence: bulk\r\n"
	return &h
}

func TestOneClickPost(t *testing.T) {
	v := newEnv(t)
	s := newOneClick(t, v)
	v.add(uuidA, "Shop <old@shop.example>", "Старое", "x", msgOpt{headers: oneClickHeaders(s.URL + "/stale")})
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "x", msgOpt{headers: oneClickHeaders(s.URL + "/old")})
	v.add(uuidA, "Shop <news@shop.example>", "Скидки 2", "x", msgOpt{headers: oneClickHeaders(s.URL + "/ok?u=1")})
	v.add(uuidB, "Deals <deals@bad.example>", "Скидки", "x", msgOpt{headers: oneClickHeaders(s.URL + "/fail")})
	v.add(uuidB, "Deals <x@redir.example>", "Скидки", "x", msgOpt{headers: oneClickHeaders(s.URL + "/to-http")})
	v.add(uuidB, "Deals <y@found.example>", "Скидки", "x", msgOpt{headers: oneClickHeaders(s.URL + "/see-other")})

	senders := []string{"news@shop.example", "@bad.example", "x@redir.example", "y@found.example"}
	p, err := v.e.BuildUnsubscribe(ctxT(t), senders, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.PlanID == "" || p.Button != "Отписаться: 4" || len(p.Unsub) != 4 {
		t.Fatalf("preview %+v", p)
	}
	for _, u := range p.Unsub {
		if u.Method != "oneclick" {
			t.Fatalf("method %+v", u)
		}
	}
	if hits, _ := s.requests(); len(hits) != 0 {
		t.Fatal("build must not touch the network")
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	hits, bodies := s.requests()
	paths := map[string]bool{}
	for i, r := range hits {
		paths[r.URL.Path] = true
		if r.Method != http.MethodPost {
			t.Fatalf("%s %s: only POST allowed", r.Method, r.URL)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || bodies[i] != "List-Unsubscribe=One-Click" {
			t.Fatalf("headers %v body %q", r.Header, bodies[i])
		}
		if r.Header.Get("User-Agent") != "mailkite/test" || r.Header.Get("Cookie") != "" {
			t.Fatalf("headers %v", r.Header)
		}
	}
	if !paths["/ok"] || paths["/old"] || paths["/stale"] || len(hits) != 4 {
		t.Fatalf("hits %v (newest message's URL only, nothing followed)", paths)
	}
	want := map[string]string{
		"news@shop.example": "news@shop.example | done",
		"@bad.example":      "@bad.example | failed: HTTP 500",
		"x@redir.example":   "x@redir.example | failed: redirect refused: not https",
		"y@found.example":   "y@found.example | failed: HTTP 302",
	}
	if len(rep.Unsubscribe) != 4 {
		t.Fatalf("report %+v", rep)
	}
	for _, line := range rep.Unsubscribe {
		sender, _, _ := strings.Cut(line, " | ")
		if !strings.HasPrefix(line, want[sender]) {
			t.Fatalf("line %q, want prefix %q", line, want[sender])
		}
	}
	logb, _ := os.ReadFile(filepath.Join(filepath.Dir(v.e.Archive.Dir), "actions.log"))
	if !strings.Contains(string(logb), "unsubscribe") || strings.Contains(string(logb), "@") || strings.Contains(string(logb), "127.0.0.1") {
		t.Fatalf("actions.log %s", logb)
	}
}

func TestOneClickRejectsHTTP(t *testing.T) {
	v := newEnv(t)
	s := newOneClick(t, v)
	plain := strings.Replace(s.URL, "https://", "http://", 1) + "/unsubscribe"
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "x", msgOpt{headers: oneClickHeaders(plain)})
	p, err := v.e.BuildUnsubscribe(ctxT(t), []string{"news@shop.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Unsub) != 1 || p.Unsub[0].Method != "manual" || p.Unsub[0].URL != plain {
		t.Fatalf("unsub %+v", p.Unsub)
	}
	if p.PlanID != "" {
		t.Fatal("nothing to do automatically: no plan")
	}
	if hits, _ := s.requests(); len(hits) != 0 || v.confirm.n != 0 {
		t.Fatal("no request, no dialog")
	}
}

// smtpRecorder is a go-smtp server on 127.0.0.1:0 recording envelopes.
type smtpRecorder struct {
	mu   sync.Mutex
	from string
	to   []string
	data string
	user string
}

type smtpSess struct{ r *smtpRecorder }

func (s smtpSess) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s smtpSess) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if pass != "pa" {
			return errors.New("bad password")
		}
		s.r.mu.Lock()
		s.r.user = user
		s.r.mu.Unlock()
		return nil
	}), nil
}
func (s smtpSess) Mail(from string, _ *smtp.MailOptions) error {
	s.r.mu.Lock()
	s.r.from = from
	s.r.mu.Unlock()
	return nil
}
func (s smtpSess) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.r.mu.Lock()
	s.r.to = append(s.r.to, to)
	s.r.mu.Unlock()
	return nil
}
func (s smtpSess) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	s.r.mu.Lock()
	s.r.data = string(b)
	s.r.mu.Unlock()
	return err
}
func (s smtpSess) Reset()        {}
func (s smtpSess) Logout() error { return nil }

func TestMailtoSends(t *testing.T) {
	v := newEnv(t)
	rec := &smtpRecorder{}
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return smtpSess{rec}, nil }))
	srv.Domain, srv.AllowInsecureAuth = "localhost", true
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	host, port, _ := net.SplitHostPort(l.Addr().String())
	pn, _ := strconv.Atoi(port)
	v.e.Cfg.Accounts[0].SMTPHost, v.e.Cfg.Accounts[0].SMTPPort, v.e.Cfg.Accounts[0].SMTPTLS = host, pn, "starttls"
	v.e.SendMail = func(ctx context.Context, a config.Account, pw, from string, to []string, raw []byte) error {
		return smtpx.SendWith(ctx, a, pw, from, to, raw, smtpx.Options{Insecure: true})
	}
	h := "Delivered-To: A@iCloud.example\r\nList-Id: <news.shop.example>\r\nList-Unsubscribe: <mailto:unsub@shop.example?subject=Remove%20me&body=I%20agree%20to%20pay%20100%24&cc=evil@x.example&bcc=evil2@x.example&to=more@x.example>\r\nPrecedence: bulk\r\n"
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "x", msgOpt{headers: &h})

	p, err := v.e.BuildUnsubscribe(ctxT(t), []string{"news@shop.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.PlanID == "" || len(p.Unsub) != 1 || p.Unsub[0].Method != "mailto" || p.Button != "Отписаться: 1" {
		t.Fatalf("preview %+v", p)
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unsubscribe) != 1 || rep.Unsubscribe[0] != "news@shop.example | done" {
		t.Fatalf("report %+v", rep)
	}
	if !strings.Contains(v.confirm.body, "письмо на unsub@shop.example с адреса a@icloud.example, тема: Remove me") {
		t.Fatalf("dialog must show the subject: %q", v.confirm.body)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.from != "a@icloud.example" || rec.user != "a@icloud.example" || len(rec.to) != 1 || rec.to[0] != "unsub@shop.example" {
		t.Fatalf("envelope from %q user %q to %v", rec.from, rec.user, rec.to)
	}
	if !strings.Contains(rec.data, "Subject: Remove me\r\n") || !strings.Contains(rec.data, "To: unsub@shop.example\r\n") ||
		strings.Contains(rec.data, "evil") || strings.Contains(rec.data, "more@") {
		t.Fatalf("data %q", rec.data)
	}
	if _, body, _ := strings.Cut(rec.data, "\r\n\r\n"); body != "unsubscribe\r\n" {
		t.Fatalf("body must be the fixed text, got %q", body)
	}
}

func TestParseMailto(t *testing.T) {
	to, subj, err := parseMailto("mailto:u%40x@shop.example")
	if err == nil {
		t.Fatalf("odd local part accepted: %q", to)
	}
	to, subj, err = parseMailto("mailto:stop@shop.example")
	if err != nil || to != "stop@shop.example" || subj != "unsubscribe" || mailtoBody != "unsubscribe" {
		t.Fatalf("%q %q %v", to, subj, err)
	}
	if _, _, err := parseMailto("mailto:a@x.example,b@y.example"); err == nil {
		t.Fatal("several recipients must be refused")
	}
	if _, _, err := parseMailto("mailto:?to=a@x.example"); err == nil {
		t.Fatal("recipient only from hfields must be refused")
	}
	_, subj, err = parseMailto("mailto:s@shop.example?subject=Hi%0D%0ABcc:%20e@x.example&body=line1%0D%0Aline2")
	if err != nil || strings.ContainsAny(subj, "\r\n") || subj != "Hi Bcc: e@x.example" {
		t.Fatalf("%q %v", subj, err)
	}
	_, subj, err = parseMailto("mailto:s@shop.example?subject=" + strings.Repeat("%D1%8F", 200))
	if err != nil || utf8.RuneCountInString(subj) > 80 {
		t.Fatalf("subject not clipped to 80: %d %v", utf8.RuneCountInString(subj), err)
	}
}

func TestUnsubscribeAndCleanupOneDialog(t *testing.T) {
	v := newEnv(t)
	s := newOneClick(t, v)
	_, id1 := v.add(uuidA, "Shop <news@shop.example>", "Скидки", "x", msgOpt{headers: oneClickHeaders(s.URL + "/ok")})
	_, id2 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 2", "x", msgOpt{headers: oneClickHeaders(s.URL + "/ok")})
	v.add(uuidA, "Shop <news@shop.example>", "Ваш код подтверждения", "Никому не сообщайте", msgOpt{headers: oneClickHeaders(s.URL + "/ok")})

	p, err := v.e.BuildUnsubscribe(ctxT(t), []string{"news@shop.example"}, "erase")
	if err != nil {
		t.Fatal(err)
	}
	if p.Button != "Отписаться: 1 и стереть 2" || p.Count != 2 || len(p.Protected) != 1 {
		t.Fatalf("preview %+v", p)
	}
	if v.e.Plans == nil || p.PlanID == "" {
		t.Fatal("no plan")
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if v.confirm.n != 1 {
		t.Fatalf("dialogs: %d, want exactly 1", v.confirm.n)
	}
	if !strings.Contains(v.confirm.body, "news@shop.example") || !strings.Contains(v.confirm.body, "Стереть") {
		t.Fatalf("dialog body %q", v.confirm.body)
	}
	if hits, _ := s.requests(); len(hits) != 1 {
		t.Fatalf("posts %d", len(hits))
	}
	srv := v.srv["a@icloud.example"]
	if srv.has("INBOX", id1) || srv.has("INBOX", id2) {
		t.Fatal("cleanup did not run")
	}
	if rep.Done != 2 || countEml(v.e.Archive.Dir) != 2 || len(rep.Unsubscribe) != 1 || rep.Unsubscribe[0] != "news@shop.example | done" {
		t.Fatalf("report %+v, eml %d", rep, countEml(v.e.Archive.Dir))
	}
}

func TestUnsubscribeCancelledDoesNothing(t *testing.T) {
	v := newEnv(t)
	s := newOneClick(t, v)
	_, id := v.add(uuidA, "Shop <news@shop.example>", "Скидки", "x", msgOpt{headers: oneClickHeaders(s.URL + "/ok")})
	v.confirm.answer = false
	p, err := v.e.BuildUnsubscribe(ctxT(t), []string{"news@shop.example"}, "erase")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil || !rep.Cancelled {
		t.Fatalf("%+v %v", rep, err)
	}
	if hits, _ := s.requests(); len(hits) != 0 || !v.srv["a@icloud.example"].has("INBOX", id) {
		t.Fatal("cancel must change nothing")
	}
	if _, err := v.e.Execute(ctxT(t), p.PlanID); err == nil {
		t.Fatal("plan is single-use")
	}
}

func TestUnsubscribeRefusesBadInput(t *testing.T) {
	v := newEnv(t)
	if _, err := v.e.BuildUnsubscribe(ctxT(t), nil, ""); err == nil {
		t.Fatal("no senders")
	}
	if _, err := v.e.BuildUnsubscribe(ctxT(t), []string{"news@shop.example"}, "move"); err == nil {
		t.Fatal("alsoCleanup is erase or trash")
	}
	p, err := v.e.BuildUnsubscribe(ctxT(t), []string{"nobody@nowhere.example"}, "")
	if err != nil || len(p.Unsub) != 1 || p.Unsub[0].Method != "none" || p.PlanID != "" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestMailtoAliasIsManual(t *testing.T) {
	v := newEnv(t)
	v.e.Cfg.Accounts[0].SMTPHost, v.e.Cfg.Accounts[0].SMTPPort = "127.0.0.1", 1
	sent := 0
	v.e.SendMail = func(context.Context, config.Account, string, string, []string, []byte) error { sent++; return nil }
	mailto := "mailto:unsub@shop.example?subject=stop"
	h := "Delivered-To: alias@privaterelay.example\r\nX-Original-To: alias@privaterelay.example\r\nCc: Someone <other@x.example>\r\nList-Unsubscribe: <" + mailto + ">\r\nPrecedence: bulk\r\n"
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "x", msgOpt{headers: &h})
	p, err := v.e.BuildUnsubscribe(ctxT(t), []string{"news@shop.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Unsub) != 1 || p.Unsub[0].Method != "manual" || p.Unsub[0].URL != mailto || p.PlanID != "" {
		t.Fatalf("preview %+v", p)
	}
	if !strings.Contains(strings.Join(p.Skipped, "\n"), "sent to an alias; mailto would reveal your main address") {
		t.Fatalf("skipped %v", p.Skipped)
	}
	if sent != 0 {
		t.Fatal("nothing may be sent")
	}
}

func TestNoAutomaticSenderWithCleanup(t *testing.T) {
	v := newEnv(t)
	none := "List-Id: <news.shop.example>\r\nPrecedence: bulk\r\n"
	v.add(uuidA, "Shop <news@shop.example>", "Скидки 2", "x", msgOpt{headers: &none})
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "Нажмите https://shop.example/unsubscribe", msgOpt{headers: &none})
	p, err := v.e.BuildUnsubscribe(ctxT(t), []string{"news@shop.example"}, "erase")
	if err != nil {
		t.Fatal(err)
	}
	if p.PlanID != "" || p.Count != 0 || len(p.BySender) != 0 || len(p.Protected) != 0 || p.Button != "" {
		t.Fatalf("preview %+v", p)
	}
	if len(p.Unsub) != 1 || p.Unsub[0].Method != "manual" || !strings.Contains(strings.Join(p.Skipped, "\n"), "use cleanup") {
		t.Fatalf("preview %+v", p)
	}
}

func TestCleanupWithUnsubscribeIsOnePlan(t *testing.T) {
	v := newEnv(t)
	s := newOneClick(t, v)
	_, id1 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 1", "x", msgOpt{headers: oneClickHeaders(s.URL + "/ok")})
	_, id2 := v.add(uuidA, "Deals <deals@other.example>", "Скидки 2", "x", msgOpt{headers: oneClickHeaders(s.URL + "/ok2")})
	_, keep := v.add(uuidA, "Shop <news@shop.example>", "Новости", "x", msgOpt{headers: oneClickHeaders(s.URL + "/ok")})
	// The selection is the filter (subject), not all mail of these senders.
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Filter: maildata.Filter{Subject: "Скидки"}, Action: "erase", Unsubscribe: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.PlanID == "" || p.Count != 2 || len(p.Unsub) != 2 || p.Button != "Отписаться: 2 и стереть 2" {
		t.Fatalf("preview %+v", p)
	}
	for _, u := range p.Unsub {
		if u.Method != MethodOneClick {
			t.Fatalf("unsub %+v", p.Unsub)
		}
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if v.confirm.n != 1 || !strings.HasPrefix(v.confirm.body, "Отписаться от 2 отправителей и стереть насовсем 2 письма.") {
		t.Fatalf("dialogs %d body %q", v.confirm.n, v.confirm.body)
	}
	if hits, _ := s.requests(); len(hits) != 2 {
		t.Fatalf("posts %d", len(hits))
	}
	srv := v.srv["a@icloud.example"]
	if srv.has("INBOX", id1) || srv.has("INBOX", id2) || !srv.has("INBOX", keep) {
		t.Fatal("exactly the selection must be erased")
	}
	if rep.Done != 2 || len(rep.Unsubscribe) != 2 {
		t.Fatalf("report %+v", rep)
	}
	if v.e.Plans.Len() != 0 {
		t.Fatal("no stray plan may be left")
	}
}

func TestCleanupUnsubscribeNeedsEraseOrTrash(t *testing.T) {
	v := newEnv(t)
	v.f.AddMailbox("imap://" + uuidA + "/Archive")
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "x", msgOpt{headers: oneClickHeaders("https://shop.example/u")})
	for _, r := range []CleanupRequest{
		{Senders: []string{"@shop.example"}, Action: "read", Unsubscribe: true},
		{Senders: []string{"@shop.example"}, Action: "move", MoveTo: "Archive", Unsubscribe: true},
	} {
		if _, err := v.e.BuildCleanup(ctxT(t), r); err == nil || !strings.Contains(err.Error(), "unsubscribe needs action erase or trash") {
			t.Fatalf("%s: %v", r.Action, err)
		}
	}
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "trash", Unsubscribe: true})
	if err != nil || p.PlanID == "" || p.Button != "Отписаться: 1 и в корзину 1" {
		t.Fatalf("trash: %+v %v", p, err)
	}
}

func TestCleanupUnsubscribeProtectedWarning(t *testing.T) {
	v := newEnv(t)
	h := oneClickHeaders("https://shop.example/u")
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "x", msgOpt{headers: h})
	code, _ := v.add(uuidA, "Shop <news@shop.example>", "Ваш код подтверждения", "Никому не сообщайте", msgOpt{headers: h})
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "erase", Unsubscribe: true, AllowProtected: []int64{code}})
	if err != nil || p.Count != 2 {
		t.Fatalf("p %+v err %v", p, err)
	}
	body := captureDialog(v)
	if _, err := v.e.Execute(ctxT(t), p.PlanID); err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(*body, "\nnews@")
	if !strings.Contains(first, "в том числе защищённых: 1") {
		t.Fatalf("warning must follow the first sentence: %q", *body)
	}
}

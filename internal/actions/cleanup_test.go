package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/imapx"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/safety"
	"github.com/kad-xyz/mailkite/internal/testfixture"
)

const (
	uuidA = "AAAAAAAA-0000-0000-0000-000000000001"
	uuidB = "BBBBBBBB-0000-0000-0000-000000000002"
	uuidP = "CCCCCCCC-0000-0000-0000-000000000003"
	uuidN = "DDDDDDDD-0000-0000-0000-000000000004" // in Mail, not in config
)

const bulk = "List-Id: <news.shop.example>\r\nList-Unsubscribe: <https://shop.example/u>\r\nPrecedence: bulk\r\n"

func norm(id string) string { return strings.Trim(strings.TrimSpace(id), "<>") }

// fakeServer is one account's IMAP state shared by all its connections.
type fakeServer struct {
	mu        sync.Mutex
	boxes     map[string]map[uint32]string // mailbox -> uid -> Message-ID
	raw       map[uint32][]byte            // uid -> FetchRaw result (nil: error)
	flags     map[uint32][]string
	next      uint32
	special   string
	calls     []string
	onErase   func(box string, ts []imapx.Target)
	locateErr error
}

func newServer() *fakeServer {
	return &fakeServer{boxes: map[string]map[uint32]string{"INBOX": {}}, raw: map[uint32][]byte{}, flags: map[uint32][]string{}, next: 100}
}

func (s *fakeServer) add(box, msgID string, raw []byte) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.boxes[box] == nil {
		s.boxes[box] = map[uint32]string{}
	}
	s.next++
	s.boxes[box][s.next] = msgID
	s.raw[s.next] = raw
	return s.next
}

func (s *fakeServer) has(box, msgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.boxes[box] {
		if norm(id) == norm(msgID) {
			return true
		}
	}
	return false
}

func (s *fakeServer) log(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *fakeServer) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

type fakeConn struct{ s *fakeServer }

func (c fakeConn) LocateMany(_ context.Context, box string, items []imapx.LocateItem) (map[string]uint32, error) {
	c.s.log("locate " + box)
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.locateErr != nil {
		return nil, c.s.locateErr
	}
	out := map[string]uint32{}
	for _, it := range items {
		if id, ok := c.s.boxes[box][it.UIDHint]; ok && norm(id) == norm(it.MessageID) {
			out[it.MessageID] = it.UIDHint
			continue
		}
		for uid, id := range c.s.boxes[box] {
			if norm(id) == norm(it.MessageID) {
				out[it.MessageID] = uid
			}
		}
	}
	return out, nil
}

func (c fakeConn) FetchRaw(_ context.Context, box string, uid uint32) ([]byte, error) {
	c.s.log(fmt.Sprintf("fetch %s %d", box, uid))
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	r, ok := c.s.raw[uid]
	if !ok || r == nil {
		return nil, errors.New("no such message")
	}
	return r, nil
}

func (c fakeConn) Erase(_ context.Context, box string, ts []imapx.Target, gmail bool) (imapx.EraseResult, error) {
	c.s.log(fmt.Sprintf("erase %s %d gmail=%v", box, len(ts), gmail))
	if c.s.onErase != nil {
		c.s.onErase(box, ts)
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	var r imapx.EraseResult
	for _, t := range ts {
		id, ok := c.s.boxes[box][t.UID]
		switch {
		case !ok:
			r.Missing = append(r.Missing, t.UID)
		case norm(id) != norm(t.MessageID):
			r.Mismatched = append(r.Mismatched, t.UID)
		default:
			delete(c.s.boxes[box], t.UID)
			r.Erased = append(r.Erased, t.UID)
		}
	}
	return r, nil
}

func (c fakeConn) Move(_ context.Context, from, to string, uids []uint32) error {
	c.s.log(fmt.Sprintf("move %s->%s %d", from, to, len(uids)))
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.boxes[to] == nil {
		c.s.boxes[to] = map[uint32]string{}
	}
	for _, u := range uids {
		c.s.boxes[to][u] = c.s.boxes[from][u]
		delete(c.s.boxes[from], u)
	}
	return nil
}

func (c fakeConn) SetFlags(_ context.Context, box string, uids []uint32, flag string, on bool) error {
	c.s.log(fmt.Sprintf("flags %s %s %v %d", box, flag, on, len(uids)))
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	for _, u := range uids {
		if on {
			c.s.flags[u] = append(c.s.flags[u], flag)
		} else {
			c.s.flags[u] = slices.DeleteFunc(c.s.flags[u], func(f string) bool { return f == flag })
		}
	}
	return nil
}

func (c fakeConn) SpecialUse(_ context.Context, use string) (string, error) {
	c.s.log("special " + use)
	return c.s.special, nil
}

func (c fakeConn) Close() error { c.s.log("close"); return nil }

type fakeConfirm struct {
	answer       bool
	err          error
	n            int
	body, button string
	title        string
}

func (f *fakeConfirm) Confirm(_ context.Context, title, body, button string) (bool, error) {
	f.n++
	f.title, f.body, f.button = title, body, button
	return f.answer, f.err
}

type fakeSecrets map[string]string

func (f fakeSecrets) Get(email string) (string, error) {
	if pw, ok := f[email]; ok {
		return pw, nil
	}
	return "", errors.New("not found")
}
func (f fakeSecrets) Set(string) error { return nil }

type fakePOP struct {
	calls   [][]string
	uuid    string
	archive string // archive dir, to count .eml at call time
	emlSeen int
}

func (p *fakePOP) Delete(_ context.Context, uuid string, ids []string) (int, error) {
	p.uuid = uuid
	p.calls = append(p.calls, slices.Clone(ids))
	p.emlSeen = countEml(p.archive)
	return len(ids), nil
}

type env struct {
	t       *testing.T
	f       *testfixture.Fixture
	e       *Engine
	srv     map[string]*fakeServer // by email
	confirm *fakeConfirm
	pop     *fakePOP
	dials   []string
	dialErr map[string]error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := testfixture.New(t)
	f.AddAccount(uuidA, "iCloud", "IMAP")
	f.AddAccount(uuidB, "Gmail", "IMAP")
	f.AddAccount(uuidP, "Work", "POP")
	f.AddAccount(uuidN, "Other", "IMAP")
	st, err := maildata.Open(maildata.Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	state, err := safety.OpenState(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	v := &env{t: t, f: f, srv: map[string]*fakeServer{"a@icloud.example": newServer(), "b@gmail.example": newServer()},
		confirm: &fakeConfirm{answer: true}, dialErr: map[string]error{}}
	arch := &safety.Archive{Dir: filepath.Join(root, "archive")}
	v.pop = &fakePOP{archive: arch.Dir}
	v.e = &Engine{
		Store: st,
		Cfg: &config.Config{MarkLimit: 20, Accounts: []config.Account{
			{MailUUID: uuidA, Email: "a@icloud.example", Kind: "imap", IMAPHost: "imap.a", IMAPPort: 993},
			{MailUUID: uuidB, Email: "b@gmail.example", Kind: "imap", IMAPHost: "imap.b", IMAPPort: 993, Gmail: true},
			{MailUUID: uuidP, Email: "p@pop.example", Kind: "pop"},
		}},
		Secrets: fakeSecrets{"a@icloud.example": "pa", "b@gmail.example": "pb"},
		Plans:   &safety.Plans{},
		Confirm: v.confirm,
		Archive: arch,
		State:   state,
		POP:     v.pop,
		Dial: func(_ context.Context, a config.Account, pw string) (IMAP, error) {
			v.dials = append(v.dials, a.Email)
			if err := v.dialErr[a.Email]; err != nil {
				return nil, err
			}
			return fakeConn{v.srv[a.Email]}, nil
		},
	}
	return v
}

func rawMsg(from, msgID, subject, headers, body string) []byte {
	return []byte("From: " + from + "\r\nTo: me@example.com\r\n" + headers + "Subject: " + subject +
		"\r\nMessage-ID: " + msgID + "\r\nDate: Mon, 09 Nov 2026 10:00:00 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

type msgOpt struct {
	partial  bool
	noServer bool
	headers  *string
	box      string
}

var seq int

// add puts a message into the fixture (and, for IMAP accounts, onto the fake server).
func (v *env) add(uuid, from, subject, body string, o msgOpt) (int64, string) {
	v.t.Helper()
	seq++
	id := fmt.Sprintf("<m%d@shop.example>", seq)
	h := bulk
	if o.headers != nil {
		h = *o.headers
	}
	box := o.box
	if box == "" {
		box = "INBOX"
	}
	raw := rawMsg(from, id, subject, h, body)
	addr := from
	if i := strings.LastIndex(from, "<"); i >= 0 {
		addr = strings.Trim(from[i:], "<>")
	}
	var uid uint32
	if email := v.email(uuid); email != "" && v.srv[email] != nil && !o.noServer {
		uid = v.srv[email].add(box, id, raw)
	}
	kind := "imap"
	if uuid == uuidP {
		kind = "pop"
	}
	stored := raw
	if o.partial {
		stored = raw[:strings.Index(string(raw), "\r\n\r\n")+4] // headers only, like a .partial.emlx
	}
	rowid := v.f.AddMessage(testfixture.FixtureMsg{
		MailboxURL: kind + "://" + uuid + "/" + strings.ReplaceAll(box, " ", "%20"), From: addr, Subject: subject, MessageID: id,
		ListID: strings.Contains(h, "List-Id"), Raw: stored, Partial: o.partial, RemoteID: uid,
		Received: time.Unix(1700000000+int64(seq), 0),
	})
	return rowid, id
}

func (v *env) email(uuid string) string {
	if a, ok := v.e.Cfg.ByMailUUID(uuid); ok {
		return a.Email
	}
	return ""
}

func (v *env) allCalls() []string {
	var out []string
	for _, s := range v.srv {
		out = append(out, s.callList()...)
	}
	return out
}

func countEml(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".eml") {
			n++
		}
		return nil
	})
	return n
}

func ctxT(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func eraseShop(v *env) Preview {
	v.t.Helper()
	p, err := v.e.BuildCleanup(ctxT(v.t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "erase"})
	if err != nil {
		v.t.Fatal(err)
	}
	return p
}

func TestBuildCleanupExcludesProtected(t *testing.T) {
	v := newEnv(t)
	news, _ := v.add(uuidA, "Shop <news@shop.example>", "Скидки недели", "Большая распродажа", msgOpt{})
	code, _ := v.add(uuidA, "Shop <news@shop.example>", "Ваш код подтверждения", "Никому не сообщайте", msgOpt{})
	none := ""
	person, _ := v.add(uuidA, "Иван Петров <ivan@example.org>", "Привет", "Как дела?", msgOpt{headers: &none})

	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Filter: maildata.Filter{Mailbox: "INBOX"}, Action: "erase"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Count != 1 || p.PlanID == "" {
		t.Fatalf("count %d plan %q", p.Count, p.PlanID)
	}
	if len(p.BySender) != 1 || p.BySender[0] != (SenderCount{"news@shop.example", 1}) {
		t.Fatalf("by sender %+v", p.BySender)
	}
	reasons := map[int64]string{}
	for _, pr := range p.Protected {
		reasons[pr.ID] = pr.Reason
	}
	if len(reasons) != 2 || reasons[code] != safety.ReasonCode || reasons[person] != safety.ReasonPerson {
		t.Fatalf("protected %+v", p.Protected)
	}
	if _, ok := reasons[news]; ok {
		t.Fatal("newsletter must not be protected")
	}
	if len(v.allCalls()) != 0 || v.confirm.n != 0 {
		t.Fatal("build must not touch the server or show a dialog")
	}
}

func TestBuildCleanupRefusesUnfiltered(t *testing.T) {
	v := newEnv(t)
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "Распродажа", msgOpt{})
	if _, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Filter: maildata.Filter{Account: uuidA}, Action: "erase"}); err == nil {
		t.Fatal("an account alone is not a filter")
	}
	if _, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "nuke"}); err == nil {
		t.Fatal("unknown action")
	}
	if _, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "move"}); err == nil {
		t.Fatal("move needs move_to")
	}
}

func TestBuildCleanupSendersAndSkipped(t *testing.T) {
	v := newEnv(t)
	v.add(uuidA, "Shop <news@shop.example>", "Скидки", "Распродажа", msgOpt{})
	v.add(uuidA, "Shop <deals@mail.shop.example>", "Скидки", "Распродажа", msgOpt{})
	v.add(uuidA, "Other <news@eshop.example>", "Скидки", "Распродажа", msgOpt{}) // not @shop.example
	v.add(uuidN, "Shop <news@shop.example>", "Скидки", "Распродажа", msgOpt{})
	v.add(uuidN, "Shop <news@shop.example>", "Скидки 2", "Распродажа", msgOpt{})
	p := eraseShop(v)
	if p.Count != 2 {
		t.Fatalf("count %d %+v", p.Count, p)
	}
	if len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0], "Other") || !strings.Contains(p.Skipped[0], "2") ||
		!strings.Contains(p.Skipped[0], "account not set up (run mailkite setup)") {
		t.Fatalf("skipped %q", p.Skipped)
	}
	p2, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"news@shop.example"}, Action: "trash"})
	if err != nil || p2.Count != 1 {
		t.Fatalf("exact address: %+v %v", p2, err)
	}
}

func TestBuildCleanupTooMany(t *testing.T) {
	v := newEnv(t)
	old := maxCleanup
	maxCleanup = 3
	t.Cleanup(func() { maxCleanup = old })
	for i := 0; i < 4; i++ {
		v.add(uuidA, "Shop <news@shop.example>", fmt.Sprintf("Скидки %d", i), "Распродажа", msgOpt{})
	}
	_, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "erase"})
	if err == nil || !strings.Contains(err.Error(), "narrow") {
		t.Fatalf("err %v", err)
	}
}

func TestExecuteCancelledDoesNothing(t *testing.T) {
	v := newEnv(t)
	_, id := v.add(uuidA, "Shop <news@shop.example>", "Скидки", "Распродажа", msgOpt{})
	p := eraseShop(v)
	v.confirm.answer = false
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil || !rep.Cancelled || rep.Done != 0 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if v.confirm.n != 1 || v.confirm.button != "Стереть 1" || v.confirm.title != "mailkite" || !strings.Contains(v.confirm.body, "news@shop.example") {
		t.Fatalf("dialog %+v", v.confirm)
	}
	if len(v.dials) != 0 || len(v.allCalls()) != 0 || countEml(v.e.Archive.Dir) != 0 {
		t.Fatalf("dials %v calls %v", v.dials, v.allCalls())
	}
	if !v.srv["a@icloud.example"].has("INBOX", id) {
		t.Fatal("message gone")
	}
	// A dialog failure is also "no".
	p = eraseShop(v)
	v.confirm.answer, v.confirm.err = true, errors.New("no GUI")
	if _, err := v.e.Execute(ctxT(t), p.PlanID); err == nil || len(v.dials) != 0 {
		t.Fatalf("dialog error must stop: %v %v", err, v.dials)
	}
}

func TestExecuteArchivesBeforeErase(t *testing.T) {
	v := newEnv(t)
	srv := v.srv["a@icloud.example"]
	_, id1 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 1", "Распродажа", msgOpt{})
	_, id2 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 2", "Распродажа", msgOpt{partial: true}) // body from IMAP
	_, id3 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 3", "Распродажа", msgOpt{partial: true})
	for u, mid := range srv.boxes["INBOX"] { // the server cannot give id3's body: no archive copy
		if mid == id3 {
			srv.raw[u] = nil
		}
	}
	var atErase int
	var erased []string
	srv.onErase = func(box string, ts []imapx.Target) {
		atErase = countEml(v.e.Archive.Dir)
		for _, t := range ts {
			erased = append(erased, t.MessageID)
		}
	}
	p := eraseShop(v)
	if p.Count != 3 {
		t.Fatalf("count %d", p.Count)
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Done != 2 || rep.Failed != 1 || rep.NotFound != 0 {
		t.Fatalf("rep %+v", rep)
	}
	if atErase != 2 {
		t.Fatalf("archive had %d copies when Erase ran, want 2", atErase)
	}
	sort.Strings(erased)
	want := []string{id1, id2}
	sort.Strings(want)
	if !slices.Equal(erased, want) {
		t.Fatalf("erased %v want %v", erased, want)
	}
	if !srv.has("INBOX", id3) || srv.has("INBOX", id1) || srv.has("INBOX", id2) {
		t.Fatal("server state")
	}
	calls := srv.callList()
	if calls[len(calls)-1] != "close" {
		t.Fatalf("connection not closed: %v", calls)
	}
	// The local copy was used for the full message; IMAP only for the partial one.
	nFetch := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "fetch") {
			nFetch++
		}
	}
	if nFetch != 2 {
		t.Fatalf("fetches %d: %v", nFetch, calls)
	}
	// Archived copies carry the account UUID and the source mailbox.
	var sides int
	filepath.WalkDir(v.e.Archive.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".eml") {
			sc, err := safety.ReadSidecar(p)
			if err != nil || sc.Account != uuidA || sc.Mailbox != "INBOX" {
				t.Errorf("sidecar %+v %v", sc, err)
			}
			b, _ := os.ReadFile(p)
			if !strings.Contains(string(b), "Распродажа") {
				t.Errorf("archived copy is not the full message: %q", b)
			}
			sides++
		}
		return nil
	})
	if sides != 2 {
		t.Fatalf("archived %d", sides)
	}
}

func TestExecuteOnlyShownSet(t *testing.T) {
	v := newEnv(t)
	srv := v.srv["a@icloud.example"]
	_, id1 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 1", "Распродажа", msgOpt{})
	_, id2 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 2", "Распродажа", msgOpt{})
	p := eraseShop(v)
	_, late := v.add(uuidA, "Shop <news@shop.example>", "Скидки 3", "Распродажа", msgOpt{})
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil || rep.Done != 2 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if srv.has("INBOX", id1) || srv.has("INBOX", id2) || !srv.has("INBOX", late) {
		t.Fatal("only the previewed messages may be erased")
	}
}

func TestExecuteNotFoundOnServer(t *testing.T) {
	v := newEnv(t)
	srv := v.srv["a@icloud.example"]
	v.add(uuidA, "Shop <news@shop.example>", "Скидки 1", "Распродажа", msgOpt{})
	_, gone := v.add(uuidA, "Shop <news@shop.example>", "Скидки 2", "Распродажа", msgOpt{noServer: true})
	v.add(uuidA, "Shop <news@shop.example>", "Скидки 3", "Распродажа", msgOpt{})
	p := eraseShop(v)
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Done != 2 || rep.NotFound != 1 || rep.Failed != 0 {
		t.Fatalf("rep %+v", rep)
	}
	if len(srv.boxes["INBOX"]) != 0 {
		t.Fatalf("left %v", srv.boxes["INBOX"])
	}
	if countEml(v.e.Archive.Dir) != 2 {
		t.Fatalf("archive %d: a message not on the server is not archived", countEml(v.e.Archive.Dir))
	}
	h, _ := v.e.State.Hidden(ctxT(t))
	if _, ok := h[gone]; ok {
		t.Fatal("not-found message must not be hidden")
	}
}

func TestExecuteAccountFailureIsolated(t *testing.T) {
	v := newEnv(t)
	v.dialErr["a@icloud.example"] = errors.New("imap: login: bad password")
	_, a1 := v.add(uuidA, "Shop <news@shop.example>", "Скидки A", "Распродажа", msgOpt{})
	_, b1 := v.add(uuidB, "Shop <news@shop.example>", "Скидки B", "Распродажа", msgOpt{})
	_, b2 := v.add(uuidB, "Shop <news@shop.example>", "Скидки B2", "Распродажа", msgOpt{box: "Promo"})
	p := eraseShop(v)
	if p.Count != 3 {
		t.Fatalf("count %d", p.Count)
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Done != 2 || rep.Failed != 1 {
		t.Fatalf("rep %+v", rep)
	}
	if !v.srv["a@icloud.example"].has("INBOX", a1) || v.srv["b@gmail.example"].has("INBOX", b1) || v.srv["b@gmail.example"].has("Promo", b2) {
		t.Fatal("server state")
	}
	var sawA bool
	for _, l := range rep.PerAccount {
		if strings.HasPrefix(l, "iCloud:") && strings.Contains(l, "bad password") {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("per account %q", rep.PerAccount)
	}
	filepath.WalkDir(v.e.Archive.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".eml") {
			if sc, _ := safety.ReadSidecar(p); sc.Account != uuidB {
				t.Errorf("archived from failed account: %+v", sc)
			}
		}
		return nil
	})
	for _, c := range v.srv["b@gmail.example"].callList() {
		if strings.HasPrefix(c, "erase") && !strings.HasSuffix(c, "gmail=true") {
			t.Errorf("gmail flag not passed: %s", c)
		}
	}
	// The log has the account UUIDs and no addresses.
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(v.e.Archive.Dir), "actions.log"))
	if !strings.Contains(string(b), uuidA) || !strings.Contains(string(b), uuidB) || strings.Contains(string(b), "@") {
		t.Fatalf("actions.log %s", b)
	}
}

func TestExecuteNoPasswordIsolated(t *testing.T) {
	v := newEnv(t)
	delete(v.e.Secrets.(fakeSecrets), "a@icloud.example")
	v.add(uuidA, "Shop <news@shop.example>", "Скидки A", "Распродажа", msgOpt{})
	v.add(uuidB, "Shop <news@shop.example>", "Скидки B", "Распродажа", msgOpt{})
	rep, err := v.e.Execute(ctxT(t), eraseShop(v).PlanID)
	if err != nil || rep.Done != 1 || rep.Failed != 1 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if !strings.Contains(strings.Join(rep.PerAccount, "\n"), "mailkite setup") {
		t.Fatalf("per account %q", rep.PerAccount)
	}
}

func TestExecuteHidesErased(t *testing.T) {
	v := newEnv(t)
	_, id1 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 1", "Распродажа", msgOpt{})
	_, id2 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 2", "Распродажа", msgOpt{})
	rep, err := v.e.Execute(ctxT(t), eraseShop(v).PlanID)
	if err != nil || rep.Done != 2 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	h, err := v.e.State.Hidden(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h[id1]; !ok {
		t.Fatalf("hidden %v", h)
	}
	if _, ok := h[id2]; !ok {
		t.Fatalf("hidden %v", h)
	}
	// Hidden messages are not offered again.
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "erase"})
	if err != nil || p.Count != 0 || p.PlanID != "" {
		t.Fatalf("rebuild %+v %v", p, err)
	}
}

func TestPlanReuseRejected(t *testing.T) {
	v := newEnv(t)
	v.add(uuidA, "Shop <news@shop.example>", "Скидки 1", "Распродажа", msgOpt{})
	p := eraseShop(v)
	if _, err := v.e.Execute(ctxT(t), p.PlanID); err != nil {
		t.Fatal(err)
	}
	dials, calls, dialogs := len(v.dials), len(v.allCalls()), v.confirm.n
	if _, err := v.e.Execute(ctxT(t), p.PlanID); !errors.Is(err, safety.ErrPlanUnknown) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := v.e.Execute(ctxT(t), "nope"); err == nil {
		t.Fatal("unknown plan")
	}
	if len(v.dials) != dials || len(v.allCalls()) != calls || v.confirm.n != dialogs {
		t.Fatal("a rejected plan must not dial or show a dialog")
	}
}

func TestExecuteTrashAndRead(t *testing.T) {
	v := newEnv(t)
	srv := v.srv["a@icloud.example"]
	srv.special = "Deleted Messages"
	_, id1 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 1", "Распродажа", msgOpt{})
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "trash"})
	if err != nil || p.Count != 1 {
		t.Fatal(p, err)
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil || rep.Done != 1 || v.confirm.button != "В корзину: 1" {
		t.Fatalf("rep %+v err %v button %q", rep, err, v.confirm.button)
	}
	if srv.has("INBOX", id1) || !srv.has("Deleted Messages", id1) || countEml(v.e.Archive.Dir) != 0 {
		t.Fatal("trash: moved, not archived")
	}
	_, id2 := v.add(uuidA, "Shop <news@shop.example>", "Скидки 2", "Распродажа", msgOpt{})
	// Mail's index still lists id1 in INBOX until it syncs: the server says not found.
	p, err = v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "read"})
	if err != nil || p.Count != 2 {
		t.Fatal(p, err)
	}
	if rep, err = v.e.Execute(ctxT(t), p.PlanID); err != nil || rep.Done != 1 || rep.NotFound != 1 || v.confirm.button != "Прочитано: 2" {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	for u, mid := range srv.boxes["INBOX"] {
		if mid == id2 && !slices.Contains(srv.flags[u], `\Seen`) {
			t.Fatal("not marked read")
		}
	}
}

func TestExecutePOPArchivesThenDeletes(t *testing.T) {
	v := newEnv(t)
	_, id1 := v.add(uuidP, "Shop <news@shop.example>", "Скидки 1", "Распродажа", msgOpt{})
	_, id2 := v.add(uuidP, "Shop <news@shop.example>", "Скидки 2", "Распродажа", msgOpt{})
	p := eraseShop(v)
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil || rep.Done != 2 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if v.pop.uuid != uuidP || len(v.pop.calls) != 1 || len(v.pop.calls[0]) != 2 || v.pop.emlSeen != 2 {
		t.Fatalf("pop %+v", v.pop)
	}
	got := slices.Clone(v.pop.calls[0])
	sort.Strings(got)
	want := []string{id1, id2}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("ids %v", got)
	}
	if len(v.dials) != 0 {
		t.Fatal("POP must not dial IMAP")
	}
	// Only erase is offered for POP.
	v.add(uuidP, "Shop <news@shop.example>", "Скидки 3", "Распродажа", msgOpt{})
	p, err = v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "read"})
	if err != nil || p.Count != 0 || p.PlanID != "" || len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0], "only erase") {
		t.Fatalf("pop read %+v", p)
	}
}

func TestMarkSmallExecutesNow(t *testing.T) {
	v := newEnv(t)
	srv := v.srv["a@icloud.example"]
	none := ""
	r1, _ := v.add(uuidA, "Иван <ivan@example.org>", "Привет", "Как дела?", msgOpt{headers: &none}) // protected: fine for mark
	r2, _ := v.add(uuidA, "Shop <news@shop.example>", "Скидки", "Распродажа", msgOpt{})
	p, now, err := v.e.BuildMark(ctxT(t), []int64{r1, r2}, "flag", "")
	if err != nil || !now || p.PlanID != "" || p.Count != 2 {
		t.Fatalf("p %+v now %v err %v", p, now, err)
	}
	if p.Result == nil || p.Result.Done != 2 {
		t.Fatalf("result %+v", p.Result)
	}
	if v.confirm.n != 0 {
		t.Fatal("small mark needs no dialog")
	}
	n := 0
	for _, f := range srv.flags {
		if slices.Contains(f, `\Flagged`) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("flagged %d", n)
	}
}

func TestMarkLargeNeedsPlan(t *testing.T) {
	v := newEnv(t)
	v.e.Cfg.MarkLimit = 2
	var ids []int64
	for i := 0; i < 3; i++ {
		id, _ := v.add(uuidA, "Shop <news@shop.example>", fmt.Sprintf("Скидки %d", i), "Распродажа", msgOpt{})
		ids = append(ids, id)
	}
	p, now, err := v.e.BuildMark(ctxT(t), ids, "read", "")
	if err != nil || now || p.PlanID == "" || p.Count != 3 {
		t.Fatalf("p %+v now %v err %v", p, now, err)
	}
	if len(v.allCalls()) != 0 {
		t.Fatal("plan must not touch the server yet")
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil || rep.Done != 3 || v.confirm.button != "Прочитано: 3" {
		t.Fatalf("rep %+v err %v button %q", rep, err, v.confirm.button)
	}
	// Moving even one message to Trash or Junk goes through the dialog.
	for _, to := range []string{"Trash", "Junk", "[Gmail]/Spam"} {
		p, now, err = v.e.BuildMark(ctxT(t), ids[:1], "move", to)
		if err != nil || now || p.PlanID == "" {
			t.Fatalf("%s: p %+v now %v err %v", to, p, now, err)
		}
	}
	if _, _, err := v.e.BuildMark(ctxT(t), ids[:1], "move", ""); err == nil {
		t.Fatal("move needs a target")
	}
	if _, _, err := v.e.BuildMark(ctxT(t), ids[:1], "erase", ""); err == nil {
		t.Fatal("mark cannot erase")
	}
}

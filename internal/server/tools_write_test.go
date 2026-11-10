package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kad-xyz/mailkite/internal/actions"
	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/fts"
	"github.com/kad-xyz/mailkite/internal/imapx"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/safety"
	"github.com/kad-xyz/mailkite/internal/testfixture"
)

const bulkHdr = "List-Id: <news.shop.example>\r\nList-Unsubscribe: <https://shop.example/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\nPrecedence: bulk\r\n"

// fakeIMAP is one fake server shared by all connections the engine opens.
type fakeIMAP struct {
	mu     sync.Mutex
	boxes  map[string]map[uint32]string // mailbox -> uid -> Message-ID
	raw    map[uint32][]byte
	fail   bool // FetchRaw fails
	fetchs []string
	next   uint32
}

func newFakeIMAP() *fakeIMAP {
	return &fakeIMAP{boxes: map[string]map[uint32]string{}, raw: map[uint32][]byte{}, next: 100}
}

func (s *fakeIMAP) add(box, msgID string, raw []byte) uint32 {
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

type fakeConn struct{ s *fakeIMAP }

func (c fakeConn) LocateMany(_ context.Context, box string, items []imapx.LocateItem) (map[string]uint32, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	out := map[string]uint32{}
	for _, it := range items {
		for uid, id := range c.s.boxes[box] {
			if id == it.MessageID {
				out[it.MessageID] = uid
			}
		}
	}
	return out, nil
}

func (c fakeConn) FetchRaw(_ context.Context, box string, uid uint32) ([]byte, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.fetchs = append(c.s.fetchs, fmt.Sprintf("%s %d", box, uid))
	if c.s.fail {
		return nil, errors.New("boom")
	}
	return c.s.raw[uid], nil
}

func (c fakeConn) Erase(_ context.Context, box string, ts []imapx.Target, _ bool) (imapx.EraseResult, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	var r imapx.EraseResult
	for _, t := range ts {
		delete(c.s.boxes[box], t.UID)
		r.Erased = append(r.Erased, t.UID)
	}
	return r, nil
}
func (c fakeConn) Move(context.Context, string, string, []uint32) error { return nil }
func (c fakeConn) SetFlags(context.Context, string, []uint32, string, bool) error {
	return nil
}
func (c fakeConn) SpecialUse(context.Context, string) (string, error) { return "Trash", nil }
func (c fakeConn) Close() error                                       { return nil }

type fakeConfirm struct {
	answer bool
	n      int
}

func (f *fakeConfirm) Confirm(context.Context, string, string, string) (bool, error) {
	f.n++
	return f.answer, nil
}

type fakeSecrets map[string]string

func (f fakeSecrets) Get(email string) (string, error) {
	if pw, ok := f[email]; ok {
		return pw, nil
	}
	return "", errors.New("not found")
}
func (f fakeSecrets) Set(string) error { return nil }

type fakePOP struct{}

func (fakePOP) Delete(context.Context, string, []string) (actions.POPResult, error) {
	return actions.POPResult{}, nil
}

type wenv struct {
	*env
	imap     *fakeIMAP
	confirm  *fakeConfirm
	stateDir string
	archive  string
}

// setupWrite builds a server whose write engine uses fakes only.
func setupWrite(t *testing.T) *wenv {
	t.Helper()
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	home := t.TempDir()
	t.Setenv("HOME", home)
	w := &wenv{imap: newFakeIMAP(), confirm: &fakeConfirm{answer: true}, stateDir: t.TempDir()}
	w.archive = filepath.Join(w.stateDir, "archive")
	cfg := &config.Config{MarkLimit: 20, Accounts: []config.Account{
		{MailUUID: "A", Email: "me@x.example", Kind: "imap", IMAPHost: "imap.x", IMAPPort: 993}}}
	plans := &safety.Plans{}
	var eng *actions.Engine
	opts := Options{DownloadDir: filepath.Join(home, "Downloads"), StateDir: w.stateDir, Version: "test",
		Engine: func(st *maildata.Store, state *safety.State) (*actions.Engine, error) {
			if eng == nil {
				eng = &actions.Engine{Store: st, Cfg: cfg, Secrets: fakeSecrets{"me@x.example": "pw"}, Plans: plans,
					Confirm: w.confirm, Archive: &safety.Archive{Dir: w.archive}, State: state, POP: fakePOP{},
					Dial: func(context.Context, config.Account, string) (actions.IMAP, error) { return fakeConn{w.imap}, nil }}
			}
			return eng, nil
		}}
	ixPath := filepath.Join(t.TempDir(), "index.sqlite")
	cs := connectOpts(t, func() (*maildata.Store, error) {
		return maildata.Open(maildata.Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	}, func() (*fts.Index, error) { return fts.Open(ixPath) }, opts)
	w.env = &env{f: f, cs: cs, dl: opts.DownloadDir}
	return w
}

// addBulk adds a non-protected newsletter message that exists on the fake server too.
func (w *wenv) addBulk(from, subj string) (int64, string) {
	id := "<" + strings.ReplaceAll(subj, " ", "-") + "@shop.example>"
	raw := []byte("From: " + from + "\r\nTo: me@x.example\r\n" + bulkHdr + "Subject: " + subj + "\r\nMessage-ID: " + id +
		"\r\nDate: Mon, 09 Nov 2026 10:00:00 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n")
	uid := w.imap.add("INBOX", id, raw)
	rowid := w.f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: from, Subject: subj, MessageID: id,
		ListID: true, Raw: raw, RemoteID: uid, Received: time.Date(2026, 3, 5, 14, 30, 0, 0, time.Local)})
	return rowid, id
}

func planID(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "plan: "); ok {
			return v
		}
	}
	t.Fatalf("no plan line in %q", out)
	return ""
}

func TestWriteToolsListBudget(t *testing.T) {
	w := setupWrite(t)
	res, err := w.cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	type hint struct{ ro, destructive bool }
	want := map[string]hint{"status": {true, false}, "find": {true, false}, "read": {true, false},
		"save_attachment": {true, false}, "senders": {true, false}, "cleanup": {true, false},
		"unsubscribe": {true, false}, "mark": {false, false}, "execute": {false, true}}
	if len(res.Tools) != len(want) {
		t.Errorf("%d tools, want %d", len(res.Tools), len(want))
	}
	for _, tl := range res.Tools {
		h, ok := want[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %s", tl.Name)
			continue
		}
		delete(want, tl.Name)
		if tl.Annotations == nil || tl.Annotations.ReadOnlyHint != h.ro {
			t.Errorf("%s: ReadOnlyHint want %v", tl.Name, h.ro)
		}
		if h.destructive != (tl.Annotations != nil && tl.Annotations.DestructiveHint != nil && *tl.Annotations.DestructiveHint) {
			t.Errorf("%s: DestructiveHint want %v", tl.Name, h.destructive)
		}
		if tl.Name == "mark" && (tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint) {
			t.Errorf("mark must set DestructiveHint=false explicitly")
		}
	}
	if len(want) > 0 {
		t.Errorf("missing tools %v", want)
	}
	b, _ := json.Marshal(res.Tools)
	t.Logf("tools/list JSON: %d chars", len(b))
	if len(b) > 10000 {
		t.Errorf("schema too big: %d", len(b))
	}
}

func TestCleanupPreviewFormat(t *testing.T) {
	w := setupWrite(t)
	for i := 1; i <= 3; i++ {
		w.addBulk("news@shop.example", fmt.Sprintf("Sale %d", i))
	}
	w.addBulk("deals@other.example", "Deal")
	// a protected one: from a person, no list markers
	w.add("Hi there", "lunch?")
	out, isErr := w.call(t, "cleanup", map[string]any{"senders": []string{"@shop.example", "@other.example", "bob@x.example"}, "action": "erase"})
	if isErr {
		t.Fatalf("error: %q", out)
	}
	id := planID(t, out)
	lines := strings.Split(out, "\n")
	if lines[0] != "plan: "+id || lines[1] != "4 messages from 2 senders" {
		t.Errorf("header lines: %q", lines[:2])
	}
	for _, want := range []string{"3 | news@shop.example", "1 | deals@other.example", "protected (kept): 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if !strings.Contains(out, " | Hi there | person") && !strings.Contains(out, "| Hi there |") {
		t.Errorf("protected line missing: %q", out)
	}
	if last := lines[len(lines)-1]; last != "confirm: execute plan_id="+id+" (a macOS dialog will ask)" {
		t.Errorf("last line %q", last)
	}
	if w.confirm.n != 0 {
		t.Error("preview must not show a dialog")
	}
	// bad action and unfiltered are tool errors
	if out, isErr := w.call(t, "cleanup", map[string]any{"action": "erase"}); !isErr {
		t.Errorf("unfiltered cleanup should be an error, got %q", out)
	}
}

func TestCleanupIncludeProtectedByID(t *testing.T) {
	w := setupWrite(t)
	pid := w.add("Hi there", "lunch?")
	out, _ := w.call(t, "cleanup", map[string]any{"ids": []int{int(pid)}, "action": "trash"})
	if !strings.Contains(out, "protected (kept): 1") || strings.Contains(out, "plan: ") {
		t.Fatalf("protected must be kept: %q", out)
	}
	out, _ = w.call(t, "cleanup", map[string]any{"ids": []int{int(pid)}, "include_protected": []int{int(pid)}, "action": "trash"})
	if !strings.Contains(out, "plan: ") || !strings.Contains(out, "1 messages from 1 senders") {
		t.Fatalf("explicit id should be included: %q", out)
	}
}

func TestExecuteCancelledText(t *testing.T) {
	w := setupWrite(t)
	w.addBulk("news@shop.example", "Sale 1")
	out, _ := w.call(t, "cleanup", map[string]any{"senders": []string{"news@shop.example"}, "action": "erase"})
	w.confirm.answer = false
	res, isErr := w.call(t, "execute", map[string]any{"plan_id": planID(t, out)})
	if isErr || res != "cancelled" {
		t.Fatalf("isErr=%v %q", isErr, res)
	}
	if res, isErr := w.call(t, "execute", map[string]any{"plan_id": "nope"}); !isErr || !strings.Contains(res, "plan") {
		t.Errorf("unknown plan: isErr=%v %q", isErr, res)
	}
}

func TestErasedHiddenFromFind(t *testing.T) {
	w := setupWrite(t)
	gone, _ := w.addBulk("news@shop.example", "Sale 1")
	w.addBulk("news@shop.example", "Sale 2")
	w.addBulk("keep@shop.example", "Keep")
	out, _ := w.call(t, "cleanup", map[string]any{"ids": []int{int(gone)}, "action": "erase"})
	res, isErr := w.call(t, "execute", map[string]any{"plan_id": planID(t, out)})
	if isErr || !strings.HasPrefix(res, "done: 1, not found: 0, failed: 0") || !strings.Contains(res, "archive: "+w.archive) {
		t.Fatalf("execute: isErr=%v %q", isErr, res)
	}
	found, _ := w.call(t, "find", map[string]any{})
	if strings.Contains(found, "Sale 1") || !strings.Contains(found, "Sale 2") || !strings.Contains(found, "Keep") {
		t.Errorf("find after erase: %q", found)
	}
	if rd, isErr := w.call(t, "read", map[string]any{"id": int(gone)}); !isErr || !strings.Contains(rd, "not found") {
		t.Errorf("read of erased: isErr=%v %q", isErr, rd)
	}
	snd, _ := w.call(t, "senders", map[string]any{"min_count": 1})
	if !strings.Contains(snd, "1 (1 unread) | news@shop.example") {
		t.Errorf("senders should count only the visible message: %q", snd)
	}
	hiddenCount := func() int {
		st, err := safety.OpenState(w.stateDir)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		h, err := st.Hidden(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(h)
	}
	if n := hiddenCount(); n != 1 {
		t.Fatalf("hidden before sync = %d", n)
	}
	// Mail syncs: the message disappears from the index; the next find unhides it.
	w.f.RemoveMessage(gone)
	w.call(t, "find", map[string]any{})
	if n := hiddenCount(); n != 0 {
		t.Errorf("hidden after Mail dropped it = %d, want 0", n)
	}
}

func TestExecuteWithoutSetup(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	t.Setenv("HOME", t.TempDir())
	cs := connectOpts(t, func() (*maildata.Store, error) {
		return maildata.Open(maildata.Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	}, func() (*fts.Index, error) { return fts.Open(filepath.Join(t.TempDir(), "i.sqlite")) },
		Options{DownloadDir: t.TempDir(), StateDir: t.TempDir()})
	e := &env{f: f, cs: cs}
	for tool, args := range map[string]map[string]any{
		"cleanup":     {"senders": []string{"a@b.c"}, "action": "trash"},
		"unsubscribe": {"senders": []string{"a@b.c"}},
		"mark":        {"ids": []int{1}, "action": "read"},
		"execute":     {"plan_id": "x"},
	} {
		out, isErr := e.call(t, tool, args)
		if isErr || !strings.Contains(out, "account not set up (run mailkite setup)") {
			t.Errorf("%s: isErr=%v %q", tool, isErr, out)
		}
	}
	// read-only tools keep working
	if out, isErr := e.call(t, "find", nil); isErr || out != "no messages" {
		t.Errorf("find: isErr=%v %q", isErr, out)
	}
}

func TestMarkAndUnsubscribePreview(t *testing.T) {
	w := setupWrite(t)
	id, _ := w.addBulk("news@shop.example", "Sale 1")
	out, isErr := w.call(t, "mark", map[string]any{"ids": []int{int(id)}, "action": "read"})
	if isErr || out != "done: 1" {
		t.Fatalf("mark: isErr=%v %q", isErr, out)
	}
	if w.confirm.n != 0 {
		t.Error("small mark must not show a dialog")
	}
	out, isErr = w.call(t, "unsubscribe", map[string]any{"senders": []string{"news@shop.example"}, "then": "trash"})
	if isErr || !strings.Contains(out, "plan: ") || !strings.Contains(out, "news@shop.example | oneclick") {
		t.Fatalf("unsubscribe: isErr=%v %q", isErr, out)
	}
}

func TestReadFetchesMissingBodyOverIMAP(t *testing.T) {
	w := setupWrite(t)
	id, _ := w.addBulk("news@shop.example", "Sale 1")
	for _, p := range globEmlx(t, w.f.Root) {
		os.Remove(p)
	}
	out, isErr := w.call(t, "read", map[string]any{"id": int(id)})
	if isErr || strings.Contains(out, "not downloaded") || !strings.Contains(out, "hello") {
		t.Fatalf("isErr=%v %q", isErr, out)
	}
	if len(w.imap.fetchs) != 1 {
		t.Errorf("fetches: %v", w.imap.fetchs)
	}
	// any failure keeps the stage-1 text
	w.imap.fail = true
	out, isErr = w.call(t, "read", map[string]any{"id": int(id)})
	if isErr || !strings.Contains(out, "body: not downloaded") {
		t.Fatalf("on failure: isErr=%v %q", isErr, out)
	}
}

func TestStateDirNeedsHome(t *testing.T) {
	t.Setenv("HOME", "")
	h := &handlers{}
	if d, err := h.stateDir(); err == nil {
		t.Fatalf("no HOME must be an error, got %q", d)
	}
	h.opts.StateDir = t.TempDir()
	if d, err := h.stateDir(); err != nil || d != h.opts.StateDir {
		t.Fatalf("explicit dir: %q %v", d, err)
	}
}

func TestCleanupWithUnsubscribeTool(t *testing.T) {
	w := setupWrite(t)
	w.addBulk("news@shop.example", "Sale 1")
	w.addBulk("news@shop.example", "Sale 2")
	out, isErr := w.call(t, "cleanup", map[string]any{"senders": []string{"@shop.example"}, "action": "read", "unsubscribe": true})
	if !isErr || !strings.Contains(out, "unsubscribe needs action erase or trash") {
		t.Fatalf("read+unsubscribe: isErr=%v %q", isErr, out)
	}
	out, isErr = w.call(t, "cleanup", map[string]any{"senders": []string{"@shop.example"}, "action": "erase", "unsubscribe": true})
	if isErr || !strings.Contains(out, "news@shop.example | oneclick") || !strings.Contains(out, "2 messages from 1 senders will also be cleaned up") {
		t.Fatalf("erase+unsubscribe: isErr=%v %q", isErr, out)
	}
	planID(t, out)
}

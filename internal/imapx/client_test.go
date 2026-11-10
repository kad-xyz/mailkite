package imapx

import (
	"bytes"
	"context"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/kad-xyz/mailkite/internal/config"
)

const (
	testUser = "me@example.com"
	testPass = "app-password"
	gmTrash  = "[Gmail]/Trash"
)

// testServer wraps imapmemserver with what it lacks for these tests: SPECIAL-USE
// attributes in LIST, counters for SEARCH and UID EXPUNGE, and a switch that makes
// the server ignore the first N UID EXPUNGE commands (to exercise the retry loop).
type testServer struct {
	addr string
	user *imapmemserver.User

	mu          sync.Mutex
	boxes       []string
	special     map[string]imap.MailboxAttr
	searches    int
	expunges    int
	dropExpunge int
}

type testOptions struct {
	caps        imap.CapSet
	boxes       []string
	special     map[string]imap.MailboxAttr
	dropExpunge int
}

func defaultCaps() imap.CapSet {
	return imap.CapSet{
		imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}, imap.CapMove: {},
		imap.CapSpecialUse: {}, imap.CapListExtended: {},
	}
}

func startServer(t *testing.T, o testOptions) *testServer {
	t.Helper()
	if o.caps == nil {
		o.caps = defaultCaps()
	}
	if len(o.boxes) == 0 {
		o.boxes = []string{"INBOX"}
	}
	ts := &testServer{special: o.special, boxes: o.boxes, dropExpunge: o.dropExpunge}
	ts.user = imapmemserver.NewUser(testUser, testPass)
	for _, b := range o.boxes {
		if err := ts.user.Create(b, nil); err != nil {
			t.Fatal(err)
		}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &testSession{ts: ts}, nil, nil
		},
		Caps:         o.caps,
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	ts.addr = ln.Addr().String()
	return ts
}

type testSession struct {
	*imapmemserver.UserSession
	ts *testServer
}

func (s *testSession) Login(username, password string) error {
	if err := s.ts.user.Login(username, password); err != nil {
		return err
	}
	s.UserSession = imapmemserver.NewUserSession(s.ts.user)
	return nil
}

func (s *testSession) List(w *imapserver.ListWriter, ref string, patterns []string, _ *imap.ListOptions) error {
	s.ts.mu.Lock()
	defer s.ts.mu.Unlock()
	for _, name := range s.ts.boxes {
		for _, p := range patterns {
			if imapserver.MatchList(name, '/', ref, p) {
				d := &imap.ListData{Mailbox: name, Delim: '/'}
				if a, ok := s.ts.special[name]; ok {
					d.Attrs = []imap.MailboxAttr{a}
				}
				if err := w.WriteList(d); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func (s *testSession) Search(kind imapserver.NumKind, c *imap.SearchCriteria, o *imap.SearchOptions) (*imap.SearchData, error) {
	s.ts.mu.Lock()
	s.ts.searches++
	s.ts.mu.Unlock()
	return s.UserSession.Search(kind, c, o)
}

func (s *testSession) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	s.ts.mu.Lock()
	s.ts.expunges++
	drop := s.ts.dropExpunge > 0
	if drop {
		s.ts.dropExpunge--
	}
	s.ts.mu.Unlock()
	if drop {
		return nil
	}
	return s.UserSession.Expunge(w, uids)
}

func (ts *testServer) counts() (searches, expunges int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.searches, ts.expunges
}

func (ts *testServer) resetCounts() {
	ts.mu.Lock()
	ts.searches, ts.expunges = 0, 0
	ts.mu.Unlock()
}

func msg(id, subject string) []byte {
	return []byte("From: a@example.com\r\nTo: me@example.com\r\nSubject: " + subject +
		"\r\nMessage-ID: <" + id + ">\r\nDate: Mon, 09 Nov 2026 10:00:00 +0000\r\n\r\nbody " + subject + "\r\n")
}

// seed appends messages and returns their UIDs in order.
func (ts *testServer) seed(t *testing.T, box string, raws ...[]byte) []uint32 {
	t.Helper()
	var uids []uint32
	for _, r := range raws {
		d, err := ts.user.Append(box, bytes.NewReader(r), &imap.AppendOptions{})
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, uint32(d.UID))
	}
	return uids
}

func (ts *testServer) dial(t *testing.T) *Conn {
	t.Helper()
	host, port, _ := net.SplitHostPort(ts.addr)
	p, _ := strconv.Atoi(port)
	a := config.Account{Email: testUser, IMAPHost: host, IMAPPort: p}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := DialWith(ctx, a, testPass, DialOptions{Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// peek inspects a mailbox through an independent client: UID -> flags.
func (ts *testServer) peek(t *testing.T, box string) map[uint32][]imap.Flag {
	t.Helper()
	c, err := imapclient.DialInsecure(ts.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login(testUser, testPass).Wait(); err != nil {
		t.Fatal(err)
	}
	sel, err := c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32][]imap.Flag{}
	if sel.NumMessages == 0 {
		return out
	}
	msgs, err := c.Fetch(imap.SeqSet{{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		out[uint32(m.UID)] = m.Flags
	}
	return out
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestLocateByHintAndBySearch(t *testing.T) {
	ts := startServer(t, testOptions{})
	uids := ts.seed(t, "INBOX", msg("a1@example.com", "A"), msg("b2@example.com", "B"))
	c := ts.dial(t)

	ts.resetCounts()
	uid, found, err := c.Locate(ctx(t), "INBOX", "<b2@example.com>", uids[1])
	if err != nil || !found || uid != uids[1] {
		t.Fatalf("hint: uid=%d found=%v err=%v", uid, found, err)
	}
	if s, _ := ts.counts(); s != 0 {
		t.Fatalf("correct hint must not SEARCH, got %d searches", s)
	}

	uid, found, err = c.Locate(ctx(t), "INBOX", " b2@example.com ", uids[0])
	if err != nil || !found || uid != uids[1] {
		t.Fatalf("wrong hint: uid=%d found=%v err=%v", uid, found, err)
	}
	if s, _ := ts.counts(); s == 0 {
		t.Fatal("wrong hint must fall back to SEARCH")
	}

	uid, found, err = c.Locate(ctx(t), "INBOX", "<missing@example.com>", 999)
	if err != nil || found || uid != 0 {
		t.Fatalf("missing: uid=%d found=%v err=%v", uid, found, err)
	}

	// A substring hit ("b2@example.com" inside "xb2@example.com") is not a match.
	ts.seed(t, "INBOX", msg("xb2@example.com.ru", "X"))
	uid, found, err = c.Locate(ctx(t), "INBOX", "<2@example.com>", 0)
	if err != nil || found {
		t.Fatalf("substring: uid=%d found=%v err=%v", uid, found, err)
	}
}

func TestEraseNonGmail(t *testing.T) {
	ts := startServer(t, testOptions{})
	uids := ts.seed(t, "INBOX", msg("a@x", "A"), msg("b@x", "B"), msg("keep@x", "K"))
	c := ts.dial(t)

	erased, err := c.Erase(ctx(t), "INBOX", uids[:2], false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(erased, uids[:2]) {
		t.Fatalf("erased=%v want %v", erased, uids[:2])
	}
	left := ts.peek(t, "INBOX")
	if len(left) != 1 {
		t.Fatalf("left=%v", left)
	}
	if _, ok := left[uids[2]]; !ok {
		t.Fatalf("untouched message gone: %v", left)
	}
	for _, f := range left[uids[2]] {
		if f == imap.FlagDeleted {
			t.Fatal("untouched message got \\Deleted")
		}
	}
}

func TestEraseLeavesOtherDeletedMessages(t *testing.T) {
	ts := startServer(t, testOptions{})
	uids := ts.seed(t, "INBOX", msg("a@x", "A"))
	other, err := ts.user.Append("INBOX", bytes.NewReader(msg("other@x", "O")),
		&imap.AppendOptions{Flags: []imap.Flag{imap.FlagDeleted}})
	if err != nil {
		t.Fatal(err)
	}
	c := ts.dial(t)
	if _, err := c.Erase(ctx(t), "INBOX", uids, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.peek(t, "INBOX")[uint32(other.UID)]; !ok {
		t.Fatal("erase removed a \\Deleted message it was not given")
	}
}

func TestEraseRefusesWithoutUIDPlus(t *testing.T) {
	ts := startServer(t, testOptions{caps: imap.CapSet{imap.CapIMAP4rev1: {}}})
	uids := ts.seed(t, "INBOX", msg("a@x", "A"))
	c := ts.dial(t)
	if _, err := c.Erase(ctx(t), "INBOX", uids, false); err == nil {
		t.Fatal("expected refusal without UIDPLUS")
	}
	if len(ts.peek(t, "INBOX")) != 1 {
		t.Fatal("message must stay")
	}
	if _, e := ts.counts(); e != 0 {
		t.Fatalf("no EXPUNGE expected, got %d", e)
	}
}

func TestEraseGmailMovesToTrashThenExpunges(t *testing.T) {
	ts := startServer(t, testOptions{
		boxes:   []string{"INBOX", gmTrash},
		special: map[string]imap.MailboxAttr{gmTrash: imap.MailboxAttrTrash},
	})
	keepTrash := ts.seed(t, gmTrash, msg("old@x", "old trash"))
	uids := ts.seed(t, "INBOX", msg("g1@x", "G1"), msg("g2@x", "G2"), msg("keep@x", "K"))
	c := ts.dial(t)

	erased, err := c.Erase(ctx(t), "INBOX", uids[:2], true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(erased, uids[:2]) {
		t.Fatalf("erased=%v want %v", erased, uids[:2])
	}
	inbox := ts.peek(t, "INBOX")
	if len(inbox) != 1 {
		t.Fatalf("inbox=%v", inbox)
	}
	trash := ts.peek(t, gmTrash)
	if len(trash) != 1 {
		t.Fatalf("trash must keep only the old message, got %v", trash)
	}
	if _, ok := trash[keepTrash[0]]; !ok {
		t.Fatalf("old trash message gone: %v", trash)
	}
}

func TestEraseGmailFallbackTrashName(t *testing.T) {
	ts := startServer(t, testOptions{boxes: []string{"INBOX", gmTrash}})
	uids := ts.seed(t, "INBOX", msg("g1@x", "G1"))
	c := ts.dial(t)
	erased, err := c.Erase(ctx(t), "INBOX", uids, true)
	if err != nil || len(erased) != 1 {
		t.Fatalf("erased=%v err=%v", erased, err)
	}
	if len(ts.peek(t, "INBOX"))+len(ts.peek(t, gmTrash)) != 0 {
		t.Fatal("message left")
	}
}

func TestEraseVerifiesAndRetries(t *testing.T) {
	ts := startServer(t, testOptions{dropExpunge: 1})
	uids := ts.seed(t, "INBOX", msg("a@x", "A"), msg("b@x", "B"))
	c := ts.dial(t)
	ts.resetCounts()

	erased, err := c.Erase(ctx(t), "INBOX", uids, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(erased, uids) {
		t.Fatalf("erased=%v", erased)
	}
	if len(ts.peek(t, "INBOX")) != 0 {
		t.Fatal("messages left")
	}
	if _, e := ts.counts(); e != 2 {
		t.Fatalf("want 2 expunge attempts, got %d", e)
	}
}

func TestEraseGivesUpAfterRetries(t *testing.T) {
	ts := startServer(t, testOptions{dropExpunge: 100})
	uids := ts.seed(t, "INBOX", msg("a@x", "A"))
	c := ts.dial(t)
	ts.resetCounts()
	erased, err := c.Erase(ctx(t), "INBOX", uids, false)
	if err == nil {
		t.Fatal("expected error when messages remain")
	}
	if len(erased) != 0 {
		t.Fatalf("erased=%v, want none", erased)
	}
	if _, e := ts.counts(); e != 3 {
		t.Fatalf("want 3 expunge attempts, got %d", e)
	}
}

func TestFetchRawDoesNotSetSeen(t *testing.T) {
	ts := startServer(t, testOptions{})
	raw := msg("r@x", "R")
	uids := ts.seed(t, "INBOX", raw)
	c := ts.dial(t)
	got, err := c.FetchRaw(ctx(t), "INBOX", uids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("raw mismatch:\n%q\n%q", got, raw)
	}
	if slices.Contains(ts.peek(t, "INBOX")[uids[0]], imap.FlagSeen) {
		t.Fatal("FetchRaw set \\Seen")
	}
	if _, err := c.FetchRaw(ctx(t), "INBOX", 999); err == nil {
		t.Fatal("expected error for a missing UID")
	}
}

func TestMoveFlagsAppend(t *testing.T) {
	ts := startServer(t, testOptions{
		boxes:   []string{"INBOX", "Archive", "Sent Messages"},
		special: map[string]imap.MailboxAttr{"Archive": imap.MailboxAttrArchive, "Sent Messages": imap.MailboxAttrSent},
	})
	uids := ts.seed(t, "INBOX", msg("m1@x", "M1"), msg("m2@x", "M2"), msg("m3@x", "M3"))
	c := ts.dial(t)

	if err := c.SetFlags(ctx(t), "INBOX", uids[:2], `\Seen`, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetFlags(ctx(t), "INBOX", uids[1:2], `\Flagged`, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetFlags(ctx(t), "INBOX", uids[1:2], `\Seen`, false); err != nil {
		t.Fatal(err)
	}
	if err := c.SetFlags(ctx(t), "INBOX", uids[:1], `\Deleted`, true); err == nil {
		t.Fatal("\\Deleted must be refused by SetFlags")
	}
	in := ts.peek(t, "INBOX")
	if !slices.Contains(in[uids[0]], imap.FlagSeen) || slices.Contains(in[uids[1]], imap.FlagSeen) ||
		!slices.Contains(in[uids[1]], imap.FlagFlagged) || len(in[uids[2]]) != 0 {
		t.Fatalf("flags=%v", in)
	}

	arch, err := c.SpecialUse(ctx(t), `\Archive`)
	if err != nil || arch != "Archive" {
		t.Fatalf("special archive=%q err=%v", arch, err)
	}
	if none, err := c.SpecialUse(ctx(t), `\Junk`); err != nil || none != "" {
		t.Fatalf("junk=%q err=%v", none, err)
	}
	if err := c.Move(ctx(t), "INBOX", arch, uids[:2]); err != nil {
		t.Fatal(err)
	}
	if n := len(ts.peek(t, "INBOX")); n != 1 {
		t.Fatalf("inbox has %d", n)
	}
	if n := len(ts.peek(t, "Archive")); n != 2 {
		t.Fatalf("archive has %d", n)
	}

	sent, _ := c.SpecialUse(ctx(t), `\Sent`)
	if err := c.Append(ctx(t), sent, msg("s@x", "S"), true); err != nil {
		t.Fatal(err)
	}
	s := ts.peek(t, "Sent Messages")
	if len(s) != 1 {
		t.Fatalf("sent=%v", s)
	}
	for _, f := range s {
		if !slices.Contains(f, imap.FlagSeen) {
			t.Fatalf("appended without \\Seen: %v", f)
		}
	}
	uid, found, err := c.Locate(ctx(t), sent, "s@x", 0)
	if err != nil || !found || uid == 0 {
		t.Fatalf("appended message not found: %d %v %v", uid, found, err)
	}
}

func TestDialWrongPassword(t *testing.T) {
	ts := startServer(t, testOptions{})
	host, port, _ := net.SplitHostPort(ts.addr)
	p, _ := strconv.Atoi(port)
	_, err := DialWith(ctx(t), config.Account{Email: testUser, IMAPHost: host, IMAPPort: p}, "bad", DialOptions{Insecure: true})
	if err == nil {
		t.Fatal("expected login error")
	}
	if err := Verify(ctx(t), config.Account{Email: testUser, IMAPHost: host, IMAPPort: p, Kind: "pop"}, testPass); err == nil {
		t.Fatal("pop must be refused")
	}
}

func TestNormalizeMessageID(t *testing.T) {
	for in, want := range map[string]string{
		"<a@b>": "a@b", " a@b ": "a@b", " <a@b> ": "a@b", "": "",
	} {
		if got := normID(in); got != want {
			t.Errorf("normID(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEraseGmailWithoutCopyUIDSparesOlderTrash(t *testing.T) {
	ts := startServer(t, testOptions{
		boxes:   []string{"INBOX", gmTrash},
		special: map[string]imap.MailboxAttr{gmTrash: imap.MailboxAttrTrash},
	})
	// The same message was trashed earlier: it must survive.
	old := ts.seed(t, gmTrash, msg("dup@x", "earlier copy"))
	uids := ts.seed(t, "INBOX", msg("dup@x", "D"))
	c := ts.dial(t)
	c.noCopyUID = true
	erased, err := c.Erase(ctx(t), "INBOX", uids, true)
	if err != nil || !slices.Equal(erased, uids) {
		t.Fatalf("erased=%v err=%v", erased, err)
	}
	trash := ts.peek(t, gmTrash)
	if _, ok := trash[old[0]]; !ok || len(trash) != 1 {
		t.Fatalf("trash=%v, want only the earlier copy %d", trash, old[0])
	}
	if len(ts.peek(t, "INBOX")) != 0 {
		t.Fatal("inbox not empty")
	}
}

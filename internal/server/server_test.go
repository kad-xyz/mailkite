package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kad-xyz/mailkite/internal/fts"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/testfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

var ctx = context.Background()

type env struct {
	f  *testfixture.Fixture
	cs *mcp.ClientSession
	dl string
}

func setup(t *testing.T) *env {
	t.Helper()
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	home := t.TempDir()
	t.Setenv("HOME", home)
	dl := filepath.Join(home, "Downloads")
	return &env{f: f, cs: connect(t, func() (*maildata.Store, error) {
		return maildata.Open(maildata.Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	}, dl), dl: dl}
}

func connect(t *testing.T, openStore func() (*maildata.Store, error), dl string) *mcp.ClientSession {
	t.Helper()
	ixPath := filepath.Join(t.TempDir(), "index.sqlite")
	return connectIndex(t, openStore, func() (*fts.Index, error) { return fts.Open(ixPath) }, dl)
}

func connectIndex(t *testing.T, openStore func() (*maildata.Store, error), openIndex func() (*fts.Index, error), dl string) *mcp.ClientSession {
	t.Helper()
	srv, closeAll := NewLazy(openStore, openIndex, Options{DownloadDir: dl})
	t.Cleanup(closeAll)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := cl.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return cs
}

func (e *env) call(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := e.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func (e *env) add(subj, body string, extra ...func(*testfixture.FixtureMsg)) int64 {
	raw := "From: Bob <bob@x.com>\r\nTo: me@x.com\r\nSubject: " + subj + "\r\nMessage-ID: <" + strings.ReplaceAll(subj, " ", "-") + "@x.com>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
	m := testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "bob@x.com", FromName: "Bob", Subject: subj,
		Raw: []byte(raw), Received: time.Date(2026, 3, 5, 14, 30, 0, 0, time.Local)}
	for _, fn := range extra {
		fn(&m)
	}
	return e.f.AddMessage(m)
}

func TestToolsListBudget(t *testing.T) {
	e := setup(t)
	res, err := e.cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"status": true, "find": true, "read": true, "save_attachment": true, "senders": true}
	for _, tl := range res.Tools {
		if !want[tl.Name] {
			t.Errorf("unexpected tool %s", tl.Name)
		}
		delete(want, tl.Name)
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s not read-only", tl.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing tools %v", want)
	}
	b, _ := json.Marshal(res.Tools)
	t.Logf("tools/list JSON: %d chars", len(b))
	if len(b) > 6000 {
		t.Errorf("schema too big: %d", len(b))
	}
}

func TestFindTool(t *testing.T) {
	e := setup(t)
	e.add("one", "b", func(m *testfixture.FixtureMsg) { m.Attachments = []string{"a.pdf"} })
	e.add("two", "b", func(m *testfixture.FixtureMsg) { m.Read = true; m.Received = m.Received.Add(-time.Hour) })
	e.add("three", "b", func(m *testfixture.FixtureMsg) { m.Read = true; m.Received = m.Received.Add(-2 * time.Hour) })
	out, isErr := e.call(t, "find", map[string]any{"limit": 2})
	if isErr {
		t.Fatal(out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || lines[2] != "more: offset=2" {
		t.Fatalf("got %q", out)
	}
	if !strings.HasSuffix(lines[0], "| 05.03 14:30 | Bob | one | 📎1 | •") {
		// id prefix varies; check the stable tail
		if !strings.Contains(lines[0], " | 05.03 14:30 | Bob | one | 📎1 | •") {
			t.Errorf("line0 %q", lines[0])
		}
	}
	if !strings.Contains(lines[1], " | 05.03 13:30 | Bob | two") || strings.Contains(lines[1], "•") {
		t.Errorf("line1 %q", lines[1])
	}
	out, _ = e.call(t, "find", map[string]any{"limit": 2, "offset": 2})
	if strings.Contains(out, "more:") || !strings.Contains(out, "three") {
		t.Errorf("page2 %q", out)
	}
	out, _ = e.call(t, "find", map[string]any{"until": "2026-03-05", "mailbox": "inbox", "unread": true})
	if !strings.Contains(out, "one") || strings.Contains(out, "two") {
		t.Errorf("filtered %q", out)
	}
	out, _ = e.call(t, "find", map[string]any{"until": "2026-03-04"})
	if out != "no messages" {
		t.Errorf("until %q", out)
	}
}

func TestFindQueryRanks(t *testing.T) {
	e := setup(t)
	e.add("aaa", "kiwi")
	b := e.add("bbb", "kiwi kiwi kiwi kiwi kiwi kiwi kiwi kiwi", func(m *testfixture.FixtureMsg) { m.Received = m.Received.Add(-time.Hour) })
	e.add("ccc", "nothing")
	out, isErr := e.call(t, "find", map[string]any{"query": "kiwi"})
	if isErr {
		t.Fatal(out)
	}
	if strings.Contains(out, "ccc") || strings.Count(out, "\n") != 1 {
		t.Fatalf("got %q", out)
	}
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(first, fmt.Sprintf("%d |", b)) || !strings.Contains(first, "bbb") {
		t.Fatalf("bbb (older, higher rank) must be first: %q", out)
	}
}

func TestReadTool(t *testing.T) {
	e := setup(t)
	raw := "From: Shop <shop@x.com>\r\nTo: me@x.com\r\nSubject: Your code\r\nMessage-ID: <abc+1@x.com>\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		`<p>Your verification code: 481516</p><a href="https://x.com/unsub?u=1">Unsubscribe</a> <a href="https://www.shop.com/deals/long/path">Deals</a>` + "\r\n"
	id := e.add("Your code", "", func(m *testfixture.FixtureMsg) { m.Raw = []byte(raw) })
	out, isErr := e.call(t, "read", map[string]any{"id": id})
	if isErr {
		t.Fatal(out)
	}
	for _, w := range []string{"From: Shop", "Subject: Your code", "open: message://%3Cabc+1@x.com%3E", "codes: 481516", "- [unsubscribe] Unsubscribe → https://x.com/unsub?u=1", "- Deals (shop.com)"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in\n%s", w, out)
		}
	}
	if strings.Contains(out, "/deals/long") {
		t.Errorf("other link URL leaked:\n%s", out)
	}
	if out, isErr = e.call(t, "read", map[string]any{"id": 9999}); !isErr {
		t.Errorf("want error, got %q", out)
	}
}

func TestReadNotDownloaded(t *testing.T) {
	e := setup(t)
	id := e.add("nofile", "x")
	for _, p := range globEmlx(t, e.f.Root) {
		os.Remove(p)
	}
	out, isErr := e.call(t, "read", map[string]any{"id": id})
	if isErr || !strings.Contains(out, "body: not downloaded") {
		t.Fatalf("isErr=%v %q", isErr, out)
	}
}

func globEmlx(t *testing.T, root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".emlx") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestReadPaging(t *testing.T) {
	e := setup(t)
	body := strings.Repeat("0123456789", 1000)
	id := e.add("long", body)
	out, _ := e.call(t, "read", map[string]any{"id": id})
	if !strings.Contains(out, body[:4000]) || strings.Contains(out, body[:4001]) || !strings.HasSuffix(out, "more: offset=4000") {
		t.Fatalf("page1 bad (len %d): tail %q", len(out), out[max(0, len(out)-60):])
	}
	out, _ = e.call(t, "read", map[string]any{"id": id, "offset": 4000})
	if !strings.Contains(out, body[4000:8000]) || !strings.HasSuffix(out, "more: offset=8000") || strings.Contains(out, "attachments:") {
		t.Fatalf("page2 bad: %q", out[max(0, len(out)-60):])
	}
	out, _ = e.call(t, "read", map[string]any{"id": id, "offset": 8000})
	if strings.Contains(out, "more:") {
		t.Errorf("page3 has more")
	}
}

func TestSaveAttachmentNoOverwrite(t *testing.T) {
	e := setup(t)
	raw := "From: a@x.com\r\nSubject: att\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nhi\r\n--B\r\nContent-Type: application/pdf; name=\"../.evil.pdf\"\r\nContent-Disposition: attachment; filename=\"../.evil.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("PDFDATA")) + "\r\n--B--\r\n"
	id := e.add("att", "", func(m *testfixture.FixtureMsg) { m.Raw = []byte(raw); m.Attachments = []string{"evil.pdf"} })
	var paths []string
	for i := 0; i < 2; i++ {
		out, isErr := e.call(t, "save_attachment", map[string]any{"id": id, "dir": e.dl})
		if isErr {
			t.Fatal(out)
		}
		paths = append(paths, out)
	}
	if paths[0] == paths[1] {
		t.Fatalf("same path twice: %v", paths)
	}
	for _, p := range paths {
		if rd, _ := filepath.EvalSymlinks(e.dl); filepath.Dir(p) != rd {
			t.Errorf("escaped dir: %s", p)
		}
		b, err := os.ReadFile(p)
		if err != nil || string(b) != "PDFDATA" {
			t.Errorf("%s: %v %q", p, err, b)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", fi.Mode())
		}
	}
	if !strings.HasSuffix(paths[1], "evil (2).pdf") {
		t.Errorf("second name %s", paths[1])
	}
}

func TestSanitizeName(t *testing.T) {
	long := strings.Repeat("é", 150) + ".pdf" // 304 bytes
	if got := sanitizeName(long, 1); len(got) > 200 || !strings.HasSuffix(got, ".pdf") || !utf8.ValidString(got) {
		t.Errorf("long name: %d bytes %q", len(got), got)
	}
	for in, want := range map[string]string{"a\nb.pdf": "a_b.pdf", "a/b/c.pdf": "c.pdf", "..\\x.txt": "x.txt", ".hidden": "hidden", "": "attachment-3", "..": "attachment-3", "/": "attachment-3"} {
		if got := sanitizeName(in, 3); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestSendersAndStatus(t *testing.T) {
	e := setup(t)
	e.add("s1", "b", func(m *testfixture.FixtureMsg) { m.ListID = true; m.UnsubType = 1 })
	e.add("s2", "b", func(m *testfixture.FixtureMsg) { m.Read = true })
	out, _ := e.call(t, "senders", nil)
	if !strings.HasPrefix(out, "2 (1 unread) | Bob bob@x.com | last 05.03") || !strings.Contains(out, "| list") || !strings.Contains(out, "| unsub") {
		t.Errorf("senders %q", out)
	}
	out, isErr := e.call(t, "status", nil)
	if isErr || !strings.Contains(out, "account Work (imap): 2 msgs, 1 unread") || !strings.Contains(out, "mail index updated ") || !strings.Contains(out, "search index: 0 docs") {
		t.Errorf("status %q", out)
	}
}

func TestNoAccessIsToolError(t *testing.T) {
	root := t.TempDir()
	locked := filepath.Join(root, "Mail")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("directory permissions not enforced (root?)")
	}
	opens := 0
	cs := connect(t, func() (*maildata.Store, error) {
		opens++
		return maildata.Open(maildata.Options{MailRoot: locked, AccountsDB: filepath.Join(root, "x")})
	}, t.TempDir())
	e := &env{cs: cs}
	for _, tool := range []string{"status", "find"} {
		out, isErr := e.call(t, tool, nil)
		if !isErr || !strings.Contains(out, "Full Disk Access") {
			t.Errorf("%s: isErr=%v %q", tool, isErr, out)
		}
	}
	if opens != 2 {
		t.Errorf("open retried %d times, want 2 (no cached failure)", opens)
	}
}

func TestSaveAttachmentDirRestricted(t *testing.T) {
	e := setup(t)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, "Library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "Library"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "Library"), filepath.Join(home, "lib2")); err != nil {
		t.Fatal(err)
	}
	raw := "From: a@x.com\r\nSubject: att\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("X")) + "\r\n--B--\r\n"
	id := e.add("att", "", func(m *testfixture.FixtureMsg) { m.Raw = []byte(raw) })
	for _, d := range []string{"rel/dir", "~", "~/Library/LaunchAgents", "~/library/LaunchAgents", "~/LIBRARY/x", "~/.ssh", "/tmp/x", "~/link/x", "~/Downloads/.hid", "~/../other", "~/GolandProjects/x", "~/Downloads/../Documents"} {
		out, isErr := e.call(t, "save_attachment", map[string]any{"id": id, "dir": d})
		if !isErr {
			t.Errorf("dir %q accepted: %s", d, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents")); err == nil {
		t.Error("LaunchAgents created")
	}
	if _, err := os.Stat(filepath.Join(home, "~")); err == nil {
		t.Error("literal ~ dir created")
	}
	out, isErr := e.call(t, "save_attachment", map[string]any{"id": id, "dir": "~/Downloads/sub"})
	if isErr || !strings.HasPrefix(out, filepath.Join(home, "Downloads", "sub")) && !strings.Contains(out, "Downloads/sub/a.pdf") {
		t.Errorf("Downloads/sub: %v %q", isErr, out)
	}
	if fi, err := os.Stat(filepath.Join(home, "Downloads", "sub")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("sub dir: %v %v", fi, err)
	}
}

func TestSaveAttachmentExtraRoots(t *testing.T) {
	e := setup(t)
	home := os.Getenv("HOME")
	for _, d := range []string{"extra", "Library/saves", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MAILKITE_SAVE_DIRS", strings.Join([]string{filepath.Join(home, "extra"), filepath.Join(home, "Library", "saves"), filepath.Join(home, ".hidden"), "relative"}, ":"))
	raw := "From: a@x.com\r\nSubject: att\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("X")) + "\r\n--B--\r\n"
	id := e.add("att", "", func(m *testfixture.FixtureMsg) { m.Raw = []byte(raw) })
	if out, isErr := e.call(t, "save_attachment", map[string]any{"id": id, "dir": "~/extra/deep"}); isErr {
		t.Errorf("configured root rejected: %s", out)
	}
	for _, d := range []string{"~/Library/saves", "~/.hidden", "~/.hidden/x", "~/other"} {
		if out, isErr := e.call(t, "save_attachment", map[string]any{"id": id, "dir": d}); !isErr {
			t.Errorf("dir %q accepted: %s", d, out)
		}
	}
}

func TestSavePartialAttachments(t *testing.T) {
	e := setup(t)
	raw := "From: a@x.com\r\nSubject: att\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
		"--B\r\nContent-Type: application/pdf; name=\"s.pdf\"\r\nContent-Disposition: attachment; filename=\"s.pdf\"\r\n\r\n\r\n" +
		"--B\r\nContent-Type: application/pdf; name=\"s.pdf\"\r\nContent-Disposition: attachment; filename=\"s.pdf\"\r\n\r\n\r\n--B--\r\n"
	id := e.add("att", "", func(m *testfixture.FixtureMsg) {
		m.Raw, m.Partial = []byte(raw), true
		m.External = []testfixture.ExternalAttachment{{Part: 3, Name: "s.pdf", Data: []byte("SECOND")}, {Part: 2, Name: "s.pdf", Data: []byte("FIRST")}}
	})
	out, isErr := e.call(t, "save_attachment", map[string]any{"id": id})
	if isErr {
		t.Fatal(out)
	}
	paths := strings.Split(out, "\n")
	if len(paths) != 2 {
		t.Fatalf("got %q", out)
	}
	for i, want := range []string{"FIRST", "SECOND"} {
		if b, _ := os.ReadFile(paths[i]); string(b) != want {
			t.Errorf("%s = %q want %q", paths[i], b, want)
		}
	}
	// no external files: error
	id2 := e.add("att2", "", func(m *testfixture.FixtureMsg) { m.Raw, m.Partial = []byte(raw), true })
	out, isErr = e.call(t, "save_attachment", map[string]any{"id": id2})
	if !isErr || !strings.Contains(out, "attachment not downloaded by Mail") {
		t.Errorf("missing external: %v %q", isErr, out)
	}
}

func TestReadCacheSeesChangedFile(t *testing.T) {
	e := setup(t)
	id := e.add("cached", "first body text")
	if out, _ := e.call(t, "read", map[string]any{"id": id}); !strings.Contains(out, "first body text") {
		t.Fatalf("first read: %q", out)
	}
	raw := "From: a@x.com\r\nSubject: cached\r\n\r\nsecond, longer body text\r\n"
	for _, p := range globEmlx(t, e.f.Root) {
		if err := os.WriteFile(p, []byte(fmt.Sprintf("%d\n%s<plist/>", len(raw), raw)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, _ := e.call(t, "read", map[string]any{"id": id}); !strings.Contains(out, "second, longer body text") {
		t.Fatalf("changed file not re-read: %q", out)
	}
}

func TestStatusSurvivesUnavailableIndex(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	cs := connectIndex(t, func() (*maildata.Store, error) {
		return maildata.Open(maildata.Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	}, func() (*fts.Index, error) { return fts.Open(filepath.Join(blocker, "index.sqlite")) }, t.TempDir())
	e := &env{f: f, cs: cs}
	e.add("one", "x")
	out, isErr := e.call(t, "status", nil)
	if isErr || !strings.Contains(out, "account Work") || !strings.Contains(out, "search index: unavailable (") {
		t.Errorf("isErr=%v %q", isErr, out)
	}
}

func TestFindFallsBackWhenIndexBusy(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	ixPath := filepath.Join(t.TempDir(), "index.sqlite")
	openStore := func() (*maildata.Store, error) {
		return maildata.Open(maildata.Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	}
	e := &env{f: f, cs: connectIndex(t, openStore, func() (*fts.Index, error) { return fts.Open(ixPath) }, t.TempDir())}
	e.add("first", "alphaword")
	if out, isErr := e.call(t, "find", map[string]any{"query": "alphaword"}); isErr || !strings.Contains(out, "first") {
		t.Fatalf("baseline: %v %q", isErr, out)
	}
	e.add("second", "betaword")
	db, err := sql.Open("sqlite", "file:"+ixPath+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin() // another process is indexing
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	out, isErr := e.call(t, "find", map[string]any{"query": "alphaword"})
	if isErr || !strings.Contains(out, "first") {
		t.Errorf("busy index: isErr=%v %q", isErr, out)
	}
}

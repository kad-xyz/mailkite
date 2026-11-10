package maildata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kad-xyz/mailkite/internal/testfixture"
)

func TestEmlxPathRule(t *testing.T) {
	f := testfixture.New(t)
	const url = "imap://U/INBOX"
	mb := ParseMailboxURL(1, url)
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: url, Subject: "s"}) // creates the mailbox tree
	store := filepath.Join(mb.Dir(f.Root), "00000000-0000-0000-0000-000000000000", "Data")
	want := map[int64]string{
		42:     filepath.Join(store, "Messages", "42.emlx"),
		5405:   filepath.Join(store, "5", "Messages", "5405.emlx"),
		123456: filepath.Join(store, "3", "2", "1", "Messages", "123456.emlx"),
	}
	for _, p := range want {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for rowid, w := range want {
		p, partial, ok := s.EmlxPath(rowid, mb)
		if !ok || partial || p != w {
			t.Errorf("rowid %d: got %s partial=%v ok=%v, want %s", rowid, p, partial, ok, w)
		}
	}
	if _, _, ok := s.EmlxPath(999999, mb); ok {
		t.Error("expected not found")
	}
}

func TestEmlxPathPartialAndWalk(t *testing.T) {
	f := testfixture.New(t)
	const url = "imap://U/INBOX"
	mb := ParseMailboxURL(1, url)
	id := f.AddMessage(testfixture.FixtureMsg{MailboxURL: url, Subject: "p", Partial: true})
	s, err := Open(Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, partial, ok := s.EmlxPath(id, mb)
	if !ok || !partial || !strings.HasSuffix(p, ".partial.emlx") {
		t.Fatalf("got %s %v %v", p, partial, ok)
	}

	// Non-standard location: found by the walk fallback.
	dir := filepath.Join(mb.Dir(f.Root), "00000000-0000-0000-0000-000000000000", "Data", "Odd", "Messages")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "77777.emlx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, partial, ok = s.EmlxPath(77777, mb)
	if !ok || partial || filepath.Base(p) != "77777.emlx" {
		t.Fatalf("walk: %s %v %v", p, partial, ok)
	}
}

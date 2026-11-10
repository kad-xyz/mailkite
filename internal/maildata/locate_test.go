package maildata

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kad-xyz/mailkite/internal/testfixture"
)

func TestOpenAndVersion(t *testing.T) {
	f := testfixture.New(t)
	if err := os.MkdirAll(f.Root+"/V11/Other", 0o755); err != nil { // no Envelope Index: ignored
		t.Fatal(err)
	}
	s, err := Open(Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Version() != "V10" {
		t.Errorf("version %q", s.Version())
	}
	if s.IndexModTime().IsZero() {
		t.Error("zero mod time")
	}
}

func TestOpenNoAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores chmod")
	}
	dir := t.TempDir() + "/Mail"
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	_, err := Open(Options{MailRoot: dir})
	if !errors.Is(err, ErrNoAccess) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "Full Disk Access") {
		t.Errorf("message lacks hint: %v", err)
	}
}

func TestReadWhileWriting(t *testing.T) {
	f := testfixture.New(t)
	s, err := Open(Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://U/INBOX", Subject: fmt.Sprint("m", i)})
		}
	}()
	for i := 0; i < 200; i++ {
		var n int
		if err := s.DB().QueryRow("SELECT count(*) FROM messages").Scan(&n); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	wg.Wait()
}

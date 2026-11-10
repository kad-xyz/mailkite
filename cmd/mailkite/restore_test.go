package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/safety"
)

type appended struct {
	mailbox string
	raw     string
	seen    bool
}

type fakeAppender struct {
	got    *[]appended
	closed *int
}

func (f fakeAppender) Append(_ context.Context, mailbox string, raw []byte, seen bool) error {
	*f.got = append(*f.got, appended{mailbox, string(raw), seen})
	return nil
}
func (f fakeAppender) Close() error { *f.closed++; return nil }

func TestRestoreAppends(t *testing.T) {
	a := safety.Archive{Dir: filepath.Join(t.TempDir(), "archive")}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	p1, err := a.Save(now, maildata.Summary{ID: 1, MessageID: "m1@x", Subject: "Один", Account: "U-IMAP"}, "Work/Old", []byte("raw one"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := a.Save(now, maildata.Summary{ID: 2, MessageID: "m2@x", Subject: "Два", Account: "U-IMAP"}, "INBOX", []byte("raw two"))
	if err != nil {
		t.Fatal(err)
	}
	p3, err := a.Save(now, maildata.Summary{ID: 3, MessageID: "m3@x", Subject: "Три", Account: "U-POP"}, "INBOX", []byte("raw three"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Accounts: []config.Account{
		{MailUUID: "U-IMAP", Email: "me@imap.example", Kind: "imap", IMAPHost: "imap.example", IMAPPort: 993},
		{MailUUID: "U-POP", Email: "me@pop.example", Kind: "pop"},
	}}
	var got []appended
	var dials, closed int
	var opened []string
	var out bytes.Buffer
	d := restoreDeps{
		cfg: cfg,
		dial: func(_ context.Context, acc config.Account) (appender, error) {
			if acc.MailUUID != "U-IMAP" {
				t.Fatalf("dial %+v", acc)
			}
			dials++
			return fakeAppender{&got, &closed}, nil
		},
		open: func(_ context.Context, p string) error { opened = append(opened, p); return nil },
		out:  &out,
	}

	// a single file
	if err := runRestore(context.Background(), d, p1); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (appended{"Work/Old", "raw one", true}) {
		t.Fatalf("%+v", got)
	}

	// the whole day directory
	got, out = nil, bytes.Buffer{}
	if err := runRestore(context.Background(), d, filepath.Dir(p1)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].raw != "raw one" || got[1] != (appended{"INBOX", "raw two", true}) {
		t.Fatalf("%+v", got)
	}
	if len(opened) != 1 || opened[0] != p3 || !filepath.IsAbs(opened[0]) {
		t.Fatalf("opened %q", opened)
	}
	if n := strings.Count(out.String(), "\n"); n != 3 {
		t.Fatalf("output %q", out.String())
	}
	if dials != 2 || closed != 2 {
		t.Fatalf("dials %d closed %d", dials, closed)
	}
	for _, p := range []string{p1, p2, p3} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("archive file removed: %v", err)
		}
	}

	// unknown account fails, without touching anything
	p4, _ := a.Save(now, maildata.Summary{ID: 4, Account: "U-GONE"}, "INBOX", []byte("raw four"))
	got = nil
	if err := runRestore(context.Background(), d, p4); err == nil || len(got) != 0 {
		t.Fatalf("unknown account: %v %v", err, got)
	}
	if err := runRestore(context.Background(), d, filepath.Join(filepath.Dir(p1), "nope.eml")); err == nil {
		t.Fatal("missing file must fail")
	}
}

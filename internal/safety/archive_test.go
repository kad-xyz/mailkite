package safety

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kad-xyz/mailkite/internal/maildata"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestArchiveSaveAndPrune(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mailkite")
	a := Archive{Dir: filepath.Join(root, "archive")}
	now := time.Date(2026, 11, 10, 15, 4, 5, 0, time.Local)
	msgDate := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s := maildata.Summary{ID: 42, MessageID: "abc@x", Date: msgDate, Subject: "../../.hidden/Счёт\x01:" + strings.Repeat("я", 100), Account: "UUID-1", Mailbox: "INBOX"}
	raw := []byte("Subject: x\r\n\r\nbody")
	p1, err := a.Save(now, s, "Archive/2026", raw)
	if err != nil {
		t.Fatal(err)
	}
	dayDir := filepath.Join(a.Dir, "2026-11-10")
	if filepath.Dir(p1) != dayDir {
		t.Fatalf("path %q", p1)
	}
	base := filepath.Base(p1)
	if !strings.HasPrefix(base, "42-") || !strings.HasSuffix(base, ".eml") || strings.ContainsAny(base, "/\\:\x01") {
		t.Fatalf("name %q", base)
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(strings.TrimPrefix(base, "42-"), ".eml")); n > 60 {
		t.Fatalf("subject part %d runes", n)
	}
	if b, _ := os.ReadFile(p1); string(b) != string(raw) {
		t.Fatalf("raw %q", b)
	}
	for _, d := range []string{root, a.Dir, dayDir} {
		if m := mode(t, d); m != 0o700 {
			t.Fatalf("%s mode %o", d, m)
		}
	}
	side := strings.TrimSuffix(p1, ".eml") + ".json"
	for _, f := range []string{p1, side} {
		if m := mode(t, f); m != 0o600 {
			t.Fatalf("%s mode %o", f, m)
		}
	}
	var meta map[string]any
	b, _ := os.ReadFile(side)
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"account": "UUID-1", "mailbox": "Archive/2026", "message_id": "abc@x", "date": msgDate.Format(time.RFC3339)}
	if len(meta) != len(want) {
		t.Fatalf("meta %v", meta)
	}
	for k, v := range want {
		if meta[k] != v {
			t.Fatalf("meta[%s] = %v, want %v", k, meta[k], v)
		}
	}
	got, err := ReadSidecar(p1)
	if err != nil || got.Account != "UUID-1" || got.Mailbox != "Archive/2026" || got.MessageID != "abc@x" {
		t.Fatalf("%+v %v", got, err)
	}

	p2, err := a.Save(now, s, "INBOX", []byte("other"))
	if err != nil || p2 == p1 || !strings.HasSuffix(p2, "-2.eml") {
		t.Fatalf("second save %q %v", p2, err)
	}
	if b, _ := os.ReadFile(p1); string(b) != string(raw) {
		t.Fatal("first file overwritten")
	}
	if p3, err := a.Save(now, s, "INBOX", []byte("third")); err != nil || !strings.HasSuffix(p3, "-3.eml") {
		t.Fatalf("third save %q %v", p3, err)
	}
	if _, err := a.Save(now, s, "INBOX", nil); err == nil {
		t.Fatal("empty raw must fail")
	}
	if p, err := a.Save(now, maildata.Summary{ID: 7, Subject: "..."}, "INBOX", raw); err != nil || filepath.Base(p) != "7-message.eml" {
		t.Fatalf("dots subject %q %v", p, err)
	}

	for _, d := range []string{"2026-10-10", "2026-10-11", "notes"} { // 31 and 30 days old
		if err := os.MkdirAll(filepath.Join(a.Dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(a.Dir, "2026-01-01"), []byte("file, not dir"), 0o600)
	if err := a.Prune(now); err != nil {
		t.Fatal(err)
	}
	for d, keep := range map[string]bool{"2026-10-10": false, "2026-10-11": true, "notes": true, "2026-11-10": true, "2026-01-01": true} {
		_, err := os.Stat(filepath.Join(a.Dir, d))
		if (err == nil) != keep {
			t.Fatalf("%s keep=%v err=%v", d, keep, err)
		}
	}
	if err := (Archive{Dir: filepath.Join(root, "missing")}).Prune(now); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultRootUsesHome(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	r, err := DefaultRoot()
	if err != nil || r != filepath.Join(h, "Library", "Application Support", "mailkite") {
		t.Fatalf("%q %v", r, err)
	}
}

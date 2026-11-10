package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/maildata"
)

type fakeStore struct{ set []string }

func (f *fakeStore) Get(string) (string, error) { return "pw", nil }
func (f *fakeStore) Set(e string) error         { f.set = append(f.set, e); return nil }

func lister(context.Context) ([]maildata.Account, error) {
	return []maildata.Account{{UUID: "U-1", Name: "iCloud", Kind: "imap"}, {UUID: "U-2", Name: "Old", Kind: "pop"}}, nil
}

func TestSetupFlow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	st := &fakeStore{}
	var verified []config.Account
	verify := func(_ context.Context, a config.Account, pw string) error {
		verified = append(verified, a)
		return nil
	}
	var out bytes.Buffer
	err := runSetup(context.Background(), strings.NewReader("1\nme@icloud.example\nicloud\n"), &out, lister, st, verify, p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil || len(c.Accounts) != 1 {
		t.Fatalf("%v %+v", err, c)
	}
	a := c.Accounts[0]
	want := config.Presets["icloud"]
	want.MailUUID, want.Email, want.Kind = "U-1", "me@icloud.example", "imap"
	if a != want {
		t.Fatalf("got %+v want %+v", a, want)
	}
	if len(st.set) != 1 || st.set[0] != "me@icloud.example" || len(verified) != 1 {
		t.Fatalf("set=%v verified=%v", st.set, verified)
	}
}

func TestSetupManualPOP(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	st := &fakeStore{}
	called := false
	verify := func(context.Context, config.Account, string) error { called = true; return nil }
	in := "2\nold@x.example\nmanual\nsmtp.x.example\n465\ntls\n"
	if err := runSetup(context.Background(), strings.NewReader(in), &bytes.Buffer{}, lister, st, verify, p); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load(p)
	a := c.Accounts[0]
	if a.Kind != "pop" || a.SMTPHost != "smtp.x.example" || a.SMTPPort != 465 || a.SMTPTLS != "tls" || a.IMAPHost != "" {
		t.Fatalf("%+v", a)
	}
	if called {
		t.Fatal("IMAP verify for POP account")
	}
}

func TestSetupBadNumber(t *testing.T) {
	err := runSetup(context.Background(), strings.NewReader("9\n"), &bytes.Buffer{}, lister, &fakeStore{}, nil, filepath.Join(t.TempDir(), "c.json"))
	if err == nil {
		t.Fatal("want error")
	}
}

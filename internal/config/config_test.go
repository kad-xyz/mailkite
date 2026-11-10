package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Accounts) != 0 || c.MarkLimit != 20 {
		t.Fatalf("got %+v", c)
	}
}

func TestSaveRoundTripPerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mk", "config.json")
	in := &Config{
		Accounts:  []Account{Presets["icloud"]},
		Rules:     Rules{ProtectedDomains: []string{"bank.example"}},
		MarkLimit: 50,
	}
	in.Accounts[0].MailUUID = "U1"
	in.Accounts[0].Email = "me@icloud.example"
	if err := in.Save(p); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file perm %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(p))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm %v", di.Mode().Perm())
	}
	out, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("got %+v want %+v", out, in)
	}
}

func TestLoadDefaultsMarkLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"Accounts":[]}`), 0o600)
	c, err := Load(p)
	if err != nil || c.MarkLimit != 20 {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestByMailUUID(t *testing.T) {
	c := &Config{Accounts: []Account{{MailUUID: "a", Email: "a@x"}, {MailUUID: "b", Email: "b@x"}}}
	if a, ok := c.ByMailUUID("b"); !ok || a.Email != "b@x" {
		t.Fatalf("%v %v", a, ok)
	}
	if _, ok := c.ByMailUUID("z"); ok {
		t.Fatal("found z")
	}
}

func TestPresets(t *testing.T) {
	ic := Presets["icloud"]
	if ic.IMAPHost != "imap.mail.me.com" || ic.IMAPPort != 993 || ic.SMTPHost != "smtp.mail.me.com" || ic.SMTPPort != 587 || ic.SMTPTLS != "starttls" || ic.Gmail {
		t.Fatalf("%+v", ic)
	}
	g := Presets["gmail"]
	if g.IMAPHost != "imap.gmail.com" || g.SMTPHost != "smtp.gmail.com" || g.SMTPPort != 465 || g.SMTPTLS != "tls" || !g.Gmail {
		t.Fatalf("%+v", g)
	}
}

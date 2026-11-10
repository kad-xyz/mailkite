package maildata

import (
	"errors"
	"testing"

	"github.com/kad-xyz/mailkite/internal/testfixture"
)

func TestRemoteID(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	id := f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "x@y.example", Subject: "s", RemoteID: 4711})
	zero := f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "x@y.example", Subject: "t"})
	s := openFix(t, f)
	if uid, err := s.RemoteID(ctx, id); err != nil || uid != 4711 {
		t.Fatalf("uid=%d err=%v", uid, err)
	}
	if uid, err := s.RemoteID(ctx, zero); err != nil || uid != 0 {
		t.Fatalf("zero: uid=%d err=%v", uid, err)
	}
	if _, err := s.RemoteID(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestNameRole(t *testing.T) {
	for name, want := range map[string]Role{
		"Trash": RoleTrash, "[Gmail]/Trash": RoleTrash, "Deleted Messages": RoleTrash, "Junk": RoleJunk,
		"[Gmail]/Spam": RoleJunk, "Archive/2026": RoleOther, "INBOX": RoleInbox, "Корзина": RoleTrash, "": RoleOther,
	} {
		if got := NameRole(name); got != want {
			t.Errorf("%q: %q want %q", name, got, want)
		}
	}
}

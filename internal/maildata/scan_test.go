package maildata

import (
	"slices"
	"testing"

	"github.com/kad-xyz/mailkite/internal/testfixture"
)

func TestScanSinceAndLiveIDs(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	a := f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "x@y.com", Subject: "one", To: []string{"p@q.com", "r@q.com"}, Attachments: []string{"f.pdf"}})
	b := f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/Trash", From: "x@y.com", Subject: "two"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "x@y.com", Subject: "dead", Deleted: true})
	s := openFix(t, f)
	var ids []int64
	err := s.ScanSince(ctx, 0, func(sm Summary, mb Mailbox, to, att []string) error {
		ids = append(ids, sm.ID)
		if sm.ID == a && (!slices.Equal(to, []string{"p@q.com", "r@q.com"}) || !slices.Equal(att, []string{"f.pdf"})) {
			t.Errorf("to=%v att=%v", to, att)
		}
		return nil
	})
	if err != nil || !slices.Equal(ids, []int64{a, b}) {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	live, _ := s.LiveIDs(ctx)
	if len(live) != 2 {
		t.Errorf("live=%v", live)
	}
	ids = nil
	s.ScanSince(ctx, a, func(sm Summary, _ Mailbox, _, _ []string) error { ids = append(ids, sm.ID); return nil })
	if !slices.Equal(ids, []int64{b}) {
		t.Errorf("after: %v", ids)
	}
}

package maildata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kad-xyz/mailkite/internal/testfixture"
)

var ctx = context.Background()

func openFix(t *testing.T, f *testfixture.Fixture) *Store {
	t.Helper()
	s, err := Open(Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func tm(n int) time.Time { return time.Unix(1700000000+int64(n)*100, 0) }

func bptr(b bool) *bool { return &b }

func TestFindDefaultOrderAndLimit(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	f.AddAccount("B", "Home", "IMAP")
	for i := 0; i < 6; i++ {
		u := "imap://A/INBOX"
		if i%2 == 1 {
			u = "imap://B/INBOX"
		}
		f.AddMessage(testfixture.FixtureMsg{MailboxURL: u, From: "x@y.example", Subject: "s", Received: tm(i)})
	}
	s := openFix(t, f)
	items, more, err := s.Find(ctx, Filter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || !more {
		t.Fatalf("got %d items more=%v", len(items), more)
	}
	if !items[0].Date.Equal(tm(5)) || !items[1].Date.Equal(tm(4)) {
		t.Errorf("order: %v %v", items[0].Date, items[1].Date)
	}
	if items[0].Account != "Home" || items[0].Mailbox != "INBOX" {
		t.Errorf("account/mailbox: %q %q", items[0].Account, items[0].Mailbox)
	}
	items, more, err = s.Find(ctx, Filter{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || more {
		t.Errorf("offset page: %d more=%v", len(items), more)
	}
	items, _, err = s.Find(ctx, Filter{Account: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Errorf("account by name: %d", len(items))
	}
}

func TestFindDedupGmail(t *testing.T) {
	f := testfixture.New(t)
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://G/%5BGmail%5D/All%20Mail", MessageID: "<a@x>", Subject: "s", Received: tm(1)})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://G/INBOX", MessageID: "<a@x>", Subject: "s", Received: tm(1)})
	s := openFix(t, f)
	items, more, err := s.Find(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || more || items[0].Mailbox != "INBOX" {
		t.Fatalf("got %+v more=%v", items, more)
	}
}

func TestFindDedupPerAccount(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	f.AddAccount("B", "Home", "IMAP")
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "n@shop.example", MessageID: "<a@x>", Subject: "s", Received: tm(1)})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://B/INBOX", From: "n@shop.example", MessageID: "<a@x>", Subject: "s", Received: tm(1)})
	s := openFix(t, f)
	items, _, err := s.Find(ctx, Filter{})
	if err != nil || len(items) != 2 {
		t.Fatalf("same Message-ID in two accounts must both show: %+v %v", items, err)
	}
	uuids := map[string]bool{items[0].AccountUUID: true, items[1].AccountUUID: true}
	if !uuids["A"] || !uuids["B"] {
		t.Fatalf("AccountUUID %+v", items)
	}
	if got, _, err := s.Get(ctx, items[0].ID); err != nil || got.AccountUUID != items[0].AccountUUID || got.Account == got.AccountUUID {
		t.Fatalf("get %+v %v", got, err)
	}
	g, err := s.Senders(ctx, Filter{}, 1)
	if err != nil || len(g) != 1 || g[0].Count != 2 {
		t.Fatalf("senders %+v %v", g, err)
	}
}

func TestFindExcludesTrashAndDeleted(t *testing.T) {
	f := testfixture.New(t)
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/Deleted%20Messages", Subject: "in trash"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", Subject: "flagged deleted", Deleted: true})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", Subject: "ok"})
	s := openFix(t, f)
	items, _, err := s.Find(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Subject != "ok" {
		t.Fatalf("default: %+v", items)
	}
	items, _, err = s.Find(ctx, Filter{Role: RoleTrash})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Subject != "in trash" {
		t.Fatalf("trash: %+v", items)
	}
}

func TestFindFilters(t *testing.T) {
	f := testfixture.New(t)
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "no-reply@gosuslugi.ru", FromName: "Госуслуги", Subject: "Выписка из реестра", Received: tm(1), Attachments: []string{"a.pdf"}})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "bob@x.example", Subject: "hello", Received: tm(5), Read: true, Flagged: true})
	s := openFix(t, f)
	check := func(name string, fl Filter, want string) {
		t.Helper()
		items, _, err := s.Find(ctx, fl)
		if err != nil {
			t.Fatal(name, err)
		}
		if len(items) != 1 || items[0].Subject != want {
			t.Errorf("%s: %+v", name, items)
		}
	}
	check("from addr", Filter{From: "GOSUSLUGI"}, "Выписка из реестра")
	check("from name", Filter{From: "госуслуги"}, "Выписка из реестра")
	check("subject", Filter{Subject: "выписка"}, "Выписка из реестра")
	check("unread", Filter{Unread: bptr(true)}, "Выписка из реестра")
	check("read", Filter{Unread: bptr(false)}, "hello")
	check("flagged", Filter{Flagged: bptr(true)}, "hello")
	check("attach", Filter{HasAttachments: bptr(true)}, "Выписка из реестра")
	check("no attach", Filter{HasAttachments: bptr(false)}, "hello")
	check("since", Filter{Since: tm(3)}, "hello")
	check("until", Filter{Until: tm(3)}, "Выписка из реестра")
	check("mailbox", Filter{Mailbox: "inbox", From: "bob"}, "hello")
	items, _, err := s.Find(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	check("ids", Filter{IDs: []int64{items[1].ID}}, items[1].Subject)
}

func TestSenders(t *testing.T) {
	f := testfixture.New(t)
	for i := 0; i < 3; i++ {
		f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "a@news.example", FromName: "News", Subject: "n", ListID: true, UnsubType: 1, Read: i == 0, Received: tm(i)})
	}
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "b@x.example", Subject: "b", Read: true, Received: tm(9)})
	s := openFix(t, f)
	g, err := s.Senders(ctx, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 2 || g[0].Address != "a@news.example" || g[0].Count != 3 || g[0].Unread != 2 || !g[0].ListID || g[0].Unsub != 1 || g[0].Name != "News" || !g[0].Last.Equal(tm(2)) {
		t.Fatalf("%+v", g)
	}
	if g[1].Address != "b@x.example" || g[1].Count != 1 || g[1].ListID {
		t.Errorf("%+v", g[1])
	}
	g, err = s.Senders(ctx, Filter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 1 || g[0].Address != "a@news.example" {
		t.Errorf("minCount: %+v", g)
	}
}

func TestGet(t *testing.T) {
	f := testfixture.New(t)
	id := f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "a@x.example", Subject: "s", MessageID: "<m@x>"})
	s := openFix(t, f)
	sm, mb, err := s.Get(ctx, id)
	if err != nil || sm.MessageID != "<m@x>" || mb.Role != RoleInbox || sm.From != "a@x.example" {
		t.Fatalf("%+v %+v %v", sm, mb, err)
	}
	if _, _, err := s.Get(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v", err)
	}
}

func TestAccountsNames(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("P1", "iCloud", "IMAP")
	f.AddChildAccount("C1", "P1", "IMAP")
	f.AddAccount("W", "Work", "IMAP")
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://C1/INBOX", Subject: "a"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://C1/INBOX", Subject: "b", Read: true})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://C1/Junk", Subject: "j"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://W/INBOX", Subject: "c"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://ZZZ/INBOX", Subject: "d"})
	s := openFix(t, f)
	acc, err := s.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Account{}
	for _, a := range acc {
		got[a.UUID] = a
	}
	if got["C1"].Name != "iCloud" || got["W"].Name != "Work" || got["ZZZ"].Name != "ZZZ" {
		t.Errorf("names: %+v", acc)
	}
	if c := got["C1"]; c.Messages != 2 || c.Unread != 1 || c.Kind != "imap" || len(c.Mailboxes) != 2 {
		t.Errorf("counts: %+v", c)
	}
}

func TestFindNegativeOffset(t *testing.T) {
	f := testfixture.New(t)
	for i := 0; i < 3; i++ {
		f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", Subject: "s", Received: tm(i)})
	}
	s := openFix(t, f)
	items, more, err := s.Find(ctx, Filter{Offset: -1, Limit: 2})
	if err != nil || len(items) != 2 || !more || !items[0].Date.Equal(tm(2)) {
		t.Fatalf("%+v more=%v err=%v", items, more, err)
	}
}

func TestFindOffsetWithDedup(t *testing.T) {
	f := testfixture.New(t)
	for i := 0; i < 4; i++ {
		id := "<m" + string(rune('a'+i)) + "@x>"
		f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://G/%5BGmail%5D/All%20Mail", MessageID: id, Subject: id, Received: tm(i)})
		f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://G/INBOX", MessageID: id, Subject: id, Received: tm(i)})
	}
	s := openFix(t, f)
	items, more, err := s.Find(ctx, Filter{Offset: 1, Limit: 2})
	if err != nil || len(items) != 2 || !more {
		t.Fatalf("%+v more=%v err=%v", items, more, err)
	}
	if items[0].MessageID != "<mc@x>" || items[1].MessageID != "<mb@x>" || items[0].Mailbox != "INBOX" {
		t.Errorf("%+v", items)
	}
	items, more, err = s.Find(ctx, Filter{Offset: 3, Limit: 2})
	if err != nil || len(items) != 1 || more || items[0].MessageID != "<ma@x>" {
		t.Errorf("%+v more=%v err=%v", items, more, err)
	}
}

func TestFindLimitCap(t *testing.T) {
	f := testfixture.New(t)
	for i := 0; i < 205; i++ {
		f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", Subject: "s", Received: tm(i)})
	}
	s := openFix(t, f)
	items, more, err := s.Find(ctx, Filter{Limit: 1000})
	if err != nil || len(items) != 200 || !more {
		t.Fatalf("len=%d more=%v err=%v", len(items), more, err)
	}
}

func TestFindLikeEscape(t *testing.T) {
	f := testfixture.New(t)
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", Subject: "50% off"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", Subject: "500 off"})
	s := openFix(t, f)
	items, _, err := s.Find(ctx, Filter{Subject: "0% o"})
	if err != nil || len(items) != 1 || items[0].Subject != "50% off" {
		t.Fatalf("%+v err=%v", items, err)
	}
}

func TestAccountsSkipAllMailCounts(t *testing.T) {
	f := testfixture.New(t)
	f.AddAccount("G", "Gmail", "IMAP")
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://G/%5BGmail%5D/All%20Mail", MessageID: "<a@x>", Subject: "s"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://G/INBOX", MessageID: "<a@x>", Subject: "s"})
	s := openFix(t, f)
	acc, err := s.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(acc) != 1 || acc[0].Messages != 1 || acc[0].Unread != 1 {
		t.Errorf("All Mail double-counted: %+v", acc)
	}
}

func TestSendersDefaultSkipsSentAndDrafts(t *testing.T) {
	f := testfixture.New(t)
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "in@x.example", Subject: "i"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/Sent%20Messages", From: "me@x.example", Subject: "s"})
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/Drafts", From: "me@x.example", Subject: "d"})
	s := openFix(t, f)
	g, err := s.Senders(ctx, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 1 || g[0].Address != "in@x.example" {
		t.Errorf("default: %+v", g)
	}
	g, err = s.Senders(ctx, Filter{Role: RoleSent}, 0)
	if err != nil || len(g) != 1 || g[0].Address != "me@x.example" {
		t.Errorf("explicit sent: %+v %v", g, err)
	}
}

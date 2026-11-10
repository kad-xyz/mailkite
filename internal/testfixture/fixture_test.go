package testfixture

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFixtureRoundTrip(t *testing.T) {
	f := New(t)
	f.AddAccount("A1", "iCloud", "IMAP")
	id := f.AddMessage(FixtureMsg{
		MailboxURL: "imap://A1/INBOX", From: "a@example.com", FromName: "Alice",
		Subject: "Hello", Received: time.Unix(1700000000, 0), To: []string{"b@example.com"},
		Attachments: []string{"x.pdf"},
	})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Root, "V10", "MailData", "Envelope Index")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tbl := range []string{"messages", "subjects", "addresses", "mailboxes", "recipients", "attachments", "message_global_data"} {
		if count(t, db, "SELECT count(*) FROM "+tbl) < 1 {
			t.Errorf("%s empty", tbl)
		}
	}
	if n := count(t, db, "SELECT count(*) FROM messages WHERE ROWID=?", id); n != 1 {
		t.Fatalf("message row missing")
	}
	p := filepath.Join(f.Root, "V10", "A1", "INBOX.mbox", "00000000-0000-0000-0000-000000000000", "Data", "Messages", "1.emlx")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 3 || b[0] < '0' || b[0] > '9' {
		t.Fatalf("bad emlx: %q", b)
	}
	adb, err := sql.Open("sqlite", "file:"+f.AccountsDB+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()
	if n := count(t, adb, "SELECT count(*) FROM ZACCOUNT a JOIN ZACCOUNTTYPE t ON t.Z_PK=a.ZACCOUNTTYPE WHERE a.ZIDENTIFIER='A1' AND t.ZACCOUNTTYPEDESCRIPTION='IMAP'"); n != 1 {
		t.Fatal("account missing")
	}
}

func TestEmlxPaths(t *testing.T) {
	f := New(t)
	const store = "00000000-0000-0000-0000-000000000000"
	cases := []struct {
		url   string
		rowid int64
		part  bool
		want  string
	}{
		{"imap://U/%5BGmail%5D/All%20Mail", 5405, false, "U/[Gmail].mbox/All Mail.mbox/" + store + "/Data/5/Messages/5405.emlx"},
		{"pop://U/", 7, false, "U/INBOX.mbox/" + store + "/Data/Messages/7.emlx"},
		{"imap://U/INBOX", 123456, true, "U/INBOX.mbox/" + store + "/Data/3/2/1/Messages/123456.partial.emlx"},
	}
	for _, c := range cases {
		got := f.emlxPath(c.url, c.rowid, c.part)
		want := filepath.Join(f.Root, "V10", filepath.FromSlash(c.want))
		if got != want {
			t.Errorf("got %s want %s", got, want)
		}
	}
}

func TestConcurrentWrite(t *testing.T) {
	f := New(t)
	f.AddAccount("A1", "x", "IMAP")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			f.AddMessage(FixtureMsg{MailboxURL: "imap://A1/INBOX", From: "a@example.com", Subject: "s"})
		}
	}()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Root, "V10", "MailData", "Envelope Index")+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 20; i++ {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM messages").Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	if n := count(t, db, "SELECT count(*) FROM messages"); n != 20 {
		t.Fatalf("got %d", n)
	}
}

package maildata

import (
	"path/filepath"
	"testing"
)

func TestParseMailboxURL(t *testing.T) {
	cases := []struct {
		url  string
		want Mailbox
	}{
		{"imap://U/%5BGmail%5D/All%20Mail", Mailbox{AccountUUID: "U", Name: "[Gmail]/All Mail", Role: "all", Kind: "imap"}},
		{"imap://U/Deleted%20Messages", Mailbox{AccountUUID: "U", Name: "Deleted Messages", Role: "trash", Kind: "imap"}},
		{"pop://U/", Mailbox{AccountUUID: "U", Name: "INBOX", Role: "inbox", Kind: "pop"}},
		{"imap://U/%D0%9E%D1%82%D0%BF%D1%80%D0%B0%D0%B2%D0%BB%D0%B5%D0%BD%D0%BD%D1%8B%D0%B5", Mailbox{AccountUUID: "U", Name: "Отправленные", Role: "sent", Kind: "imap"}},
		{"imap://U/Projects/x", Mailbox{AccountUUID: "U", Name: "Projects/x", Role: "other", Kind: "imap"}},
	}
	for _, c := range cases {
		c.want.ID, c.want.URL = 7, c.url
		if got := ParseMailboxURL(7, c.url); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.url, got, c.want)
		}
	}
}

func TestMailboxDir(t *testing.T) {
	mb := ParseMailboxURL(1, "imap://U/%5BGmail%5D/All%20Mail")
	want := filepath.Join("/r", "V10", "U", "[Gmail].mbox", "All Mail.mbox")
	if got := mb.Dir("/r"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	pop := ParseMailboxURL(2, "pop://U/")
	if got, want := pop.Dir("/r"), filepath.Join("/r", "V10", "U", "INBOX.mbox"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

package maildata

import (
	"net/url"
	"path/filepath"
	"strings"
)

// Role is the semantic role of a mailbox.
type Role string

const (
	RoleInbox   Role = "inbox"
	RoleSent    Role = "sent"
	RoleDrafts  Role = "drafts"
	RoleTrash   Role = "trash"
	RoleJunk    Role = "junk"
	RoleArchive Role = "archive"
	RoleAll     Role = "all"
	RoleOther   Role = "other"
)

var roleByName = map[string]Role{
	"inbox":            RoleInbox,
	"sent messages":    RoleSent,
	"sent mail":        RoleSent,
	"sent":             RoleSent,
	"отправленные":     RoleSent,
	"drafts":           RoleDrafts,
	"черновики":        RoleDrafts,
	"deleted messages": RoleTrash,
	"trash":            RoleTrash,
	"корзина":          RoleTrash,
	"junk":             RoleJunk,
	"spam":             RoleJunk,
	"спам":             RoleJunk,
	"archive":          RoleArchive,
	"all mail":         RoleAll,
}

// Mailbox describes one row of the mailboxes table.
type Mailbox struct {
	ID          int64
	URL         string
	AccountUUID string
	Name        string // percent-decoded path, segments joined by "/"
	Role        Role
	Kind        string // imap|pop|local|ews
}

// ParseMailboxURL splits a mailbox URL such as imap://UUID/%5BGmail%5D/All%20Mail.
func ParseMailboxURL(id int64, rawURL string) Mailbox {
	mb := Mailbox{ID: id, URL: rawURL, Role: RoleOther}
	rest := rawURL
	if i := strings.Index(rest, "://"); i >= 0 {
		mb.Kind = strings.ToLower(rest[:i])
		rest = rest[i+3:]
	}
	host, path, _ := strings.Cut(rest, "/")
	mb.AccountUUID = host
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s == "" {
			continue
		}
		if d, err := url.PathUnescape(s); err == nil {
			s = d
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		segs = []string{"INBOX"}
	}
	mb.Name = strings.Join(segs, "/")
	if r, ok := roleByName[strings.ToLower(segs[len(segs)-1])]; ok {
		mb.Role = r
	}
	return mb
}

// Dir returns the mailbox directory under mailRoot (version V10 layout).
func (m Mailbox) Dir(mailRoot string) string {
	return m.dirIn(filepath.Join(mailRoot, "V10"))
}

func (m Mailbox) dirIn(versionDir string) string {
	parts := []string{versionDir, m.AccountUUID}
	for _, s := range strings.Split(m.Name, "/") {
		parts = append(parts, s+".mbox")
	}
	return filepath.Join(parts...)
}

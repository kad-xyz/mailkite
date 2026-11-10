package server

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kad-xyz/mailkite/internal/maildata"
)

// oneLine collapses whitespace (including newlines) to single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func parseDay(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad date %q: use YYYY-MM-DD", s)
	}
	return t, nil
}

// dateRange parses since/until; until is inclusive of its whole day.
func dateRange(since, until string) (s, u time.Time, err error) {
	if since != "" {
		if s, err = parseDay(since); err != nil {
			return
		}
	}
	if until != "" {
		if u, err = parseDay(until); err != nil {
			return
		}
		u = u.AddDate(0, 0, 1)
	}
	return
}

var roleNames = map[string]maildata.Role{
	"inbox": maildata.RoleInbox, "sent": maildata.RoleSent, "drafts": maildata.RoleDrafts,
	"trash": maildata.RoleTrash, "junk": maildata.RoleJunk, "archive": maildata.RoleArchive, "all": maildata.RoleAll,
}

// mailboxFilter maps the mailbox parameter to a role or a mailbox name.
func mailboxFilter(f *maildata.Filter, mailbox string) {
	if r, ok := roleNames[strings.ToLower(strings.TrimSpace(mailbox))]; ok {
		f.Role = r
	} else {
		f.Mailbox = mailbox
	}
}

func displayName(name, addr string) string {
	if n := oneLine(name); n != "" {
		return n
	}
	return addr
}

func summaryLine(sm maildata.Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d | %s | %s | %s", sm.ID, sm.Date.Local().Format("02.01 15:04"),
		displayName(sm.FromName, sm.From), oneLine(sm.Subject))
	if sm.Attachments > 0 {
		fmt.Fprintf(&b, " | 📎%d", sm.Attachments)
	}
	if !sm.Read {
		b.WriteString(" | •")
	}
	return b.String()
}

func messageLink(id string) string {
	id = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "<"), ">")
	return "message://%3C" + url.PathEscape(id) + "%3E"
}

func sizeKB(n int64) string {
	kb := (n + 1023) / 1024
	if kb == 0 && n > 0 {
		kb = 1
	}
	return strconv.FormatInt(kb, 10) + " KB"
}

func domainOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

// sanitizeName makes an attachment name safe to use as a file name.
func sanitizeName(name string, index int) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.ReplaceAll(name, "\x00", "")
	name = filepath.Base(strings.TrimSpace(name))
	if name == "/" || name == "." {
		name = ""
	}
	name = strings.TrimSpace(strings.TrimLeft(name, "."))
	if name == "" {
		return "attachment-" + strconv.Itoa(index)
	}
	return name
}

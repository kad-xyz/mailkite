package server

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

// maxNameBytes caps a saved file name.
const maxNameBytes = 200

// sanitizeName makes an attachment name safe to use as a file name.
func sanitizeName(name string, index int) string {
	name = strings.ToValidUTF8(name, "_")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(strings.TrimSpace(name))
	if name == "/" || name == "." {
		name = ""
	}
	name = strings.TrimSpace(strings.TrimLeft(name, "."))
	if name == "" {
		return "attachment-" + strconv.Itoa(index)
	}
	if len(name) > maxNameBytes {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		stem := name[:len(name)-len(ext)]
		cut := maxNameBytes - len(ext)
		for cut > 0 && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		name = stem[:cut] + ext
	}
	return name
}

// saveRoots lists the folders save_attachment may write into: the download directory
// plus any absolute paths from MAILKITE_SAVE_DIRS (colon-separated). Each root goes
// through the same checks as a requested dir; invalid ones are dropped.
func saveRoots(downloadDir string) []string {
	var roots []string
	cands := []string{downloadDir}
	if env := os.Getenv("MAILKITE_SAVE_DIRS"); env != "" {
		cands = append(cands, strings.Split(env, ":")...)
	}
	for _, c := range cands {
		if c == "" || (!filepath.IsAbs(c) && !strings.HasPrefix(c, "~/")) {
			continue
		}
		if r, err := cleanDir(c); err == nil {
			roots = append(roots, r)
		}
	}
	return roots
}

// resolveDir validates a user-supplied save directory: it must pass cleanDir and lie
// inside one of the allowed roots (see saveRoots).
func resolveDir(dir string, roots []string) (string, error) {
	full, err := cleanDir(dir)
	if err != nil {
		return "", err
	}
	for _, r := range roots {
		if rel, err := filepath.Rel(r, full); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return full, nil
		}
	}
	return "", errors.New("dir must be inside the downloads folder (or a folder from MAILKITE_SAVE_DIRS)")
}

// cleanDir expands ~, requires an absolute path, resolves symlinks, and rejects anything
// outside $HOME, inside ~/Library, or with a dot-prefixed component.
func cleanDir(dir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot determine home directory")
	}
	rhome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", errors.New("cannot resolve home directory")
	}
	if dir == "~" {
		dir = home
	} else if strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(home, dir[2:])
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("dir must be an absolute path or start with ~/")
	}
	dir = filepath.Clean(dir)
	// resolve symlinks of the deepest existing ancestor
	existing, rest := dir, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", errors.New("cannot resolve dir")
	}
	full := filepath.Join(real, rest)
	rel, err := filepath.Rel(rhome, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New("dir must be inside your home directory")
	}
	if rel != "." {
		for i, seg := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasPrefix(seg, ".") {
				return "", errors.New("dir must not be a hidden folder")
			}
			if i == 0 && isLibrary(rhome, seg) {
				return "", errors.New("dir must not be inside ~/Library")
			}
		}
	}
	return full, nil
}

// isLibrary reports whether the first component under home is ~/Library, also on
// case-insensitive volumes where "library" or "LIBRARY" is the same folder.
func isLibrary(home, seg string) bool {
	if strings.EqualFold(seg, "Library") {
		return true
	}
	lib, err := os.Stat(filepath.Join(home, "Library"))
	if err != nil {
		return false
	}
	cur, err := os.Stat(filepath.Join(home, seg))
	return err == nil && os.SameFile(lib, cur)
}

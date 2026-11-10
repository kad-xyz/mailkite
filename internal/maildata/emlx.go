package maildata

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var uuidRe = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// EmlxPath finds the .emlx file of a message. partial reports a .partial.emlx.
func (s *Store) EmlxPath(rowid int64, mb Mailbox) (path string, partial bool, ok bool) {
	dir := mb.dirIn(filepath.Join(s.root, s.version))
	if store := storeDir(dir); store != "" {
		base := filepath.Join(store, "Data")
		if k := rowid / 1000; k > 0 {
			digits := strconv.FormatInt(k, 10)
			for i := len(digits) - 1; i >= 0; i-- {
				base = filepath.Join(base, digits[i:i+1])
			}
		}
		base = filepath.Join(base, "Messages")
		id := strconv.FormatInt(rowid, 10)
		for _, c := range []struct {
			name    string
			partial bool
		}{{id + ".emlx", false}, {id + ".partial.emlx", true}} {
			p := filepath.Join(base, c.name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, c.partial, true
			}
		}
	}
	return s.walkLookup(dir, rowid)
}

// storeDir returns the single UUID-named subfolder of a mailbox dir, or "".
func storeDir(mboxDir string) string {
	entries, err := os.ReadDir(mboxDir)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		if e.IsDir() && uuidRe.MatchString(e.Name()) {
			if found != "" {
				return ""
			}
			found = e.Name()
		}
	}
	if found == "" {
		return ""
	}
	return filepath.Join(mboxDir, found)
}

// walkLookup walks the mailbox dir once and caches rowid -> file for the Store lifetime.
func (s *Store) walkLookup(dir string, rowid int64) (string, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.walkedDir[dir] {
		m := map[int64]walked{}
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name, isPartial := strings.CutSuffix(d.Name(), ".partial.emlx")
			if !isPartial {
				var okSuffix bool
				if name, okSuffix = strings.CutSuffix(d.Name(), ".emlx"); !okSuffix {
					return nil
				}
			}
			id, err := strconv.ParseInt(name, 10, 64)
			if err != nil {
				return nil
			}
			if old, dup := m[id]; dup && !old.partial {
				return nil // prefer a full message
			}
			m[id] = walked{p, isPartial}
			return nil
		})
		s.walkCache[dir] = m
		s.walkedDir[dir] = true
	}
	w, ok := s.walkCache[dir][rowid]
	return w.path, w.partial, ok
}

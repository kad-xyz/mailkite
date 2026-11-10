// Package maildata locates Apple Mail's local data and reads it read-only.
package maildata

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// ErrNoAccess means the Mail directory cannot be read (no Full Disk Access).
var ErrNoAccess = errors.New("cannot read the Mail data directory: macOS denied access. " +
	"Grant Full Disk Access: System Settings > Privacy & Security > Full Disk Access, " +
	"enable the app that launches this server (the terminal app when used from Claude Code), " +
	"then fully quit and reopen that app")

// Options configures Open. Empty fields use the default macOS locations.
type Options struct {
	MailRoot, AccountsDB string
}

type walked struct {
	path    string
	partial bool
}

// Store gives read-only access to the Mail index and message files.
type Store struct {
	root, accountsDB, version string
	db                        *sql.DB

	mu        sync.Mutex
	walkedDir map[string]bool
	walkCache map[string]map[int64]walked // mailbox dir -> rowid -> file
}

var versionRe = regexp.MustCompile(`^V(\d+)$`)

// Open locates the newest Mail data version and opens its Envelope Index read-only.
func Open(opt Options) (*Store, error) {
	if opt.MailRoot == "" || opt.AccountsDB == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if opt.MailRoot == "" {
			opt.MailRoot = filepath.Join(home, "Library", "Mail")
		}
		if opt.AccountsDB == "" {
			opt.AccountsDB = filepath.Join(home, "Library", "Accounts", "Accounts4.sqlite")
		}
	}
	entries, err := os.ReadDir(opt.MailRoot)
	if err != nil {
		return nil, wrapAccess(err, opt.MailRoot)
	}
	best, bestN := "", -1
	for _, e := range entries {
		m := versionRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n <= bestN {
			continue
		}
		if _, err := os.Stat(indexPath(opt.MailRoot, e.Name())); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return nil, wrapAccess(err, opt.MailRoot)
			}
			continue
		}
		best, bestN = e.Name(), n
	}
	if best == "" {
		return nil, fmt.Errorf("no Mail data (V<n>/MailData/Envelope Index) found in %s", opt.MailRoot)
	}
	db, err := openReadOnly(indexPath(opt.MailRoot, best))
	if err != nil {
		return nil, wrapAccess(err, opt.MailRoot)
	}
	return &Store{
		root: opt.MailRoot, accountsDB: opt.AccountsDB, version: best, db: db,
		walkedDir: map[string]bool{}, walkCache: map[string]map[int64]walked{},
	}, nil
}

func indexPath(root, version string) string {
	return filepath.Join(root, version, "MailData", "Envelope Index")
}

func wrapAccess(err error, root string) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w (%s): %v", ErrNoAccess, root, err)
	}
	return err
}

// Close releases the index handle.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the read-only Envelope Index handle.
func (s *Store) DB() *sql.DB { return s.db }

// MailRoot returns the Mail directory in use.
func (s *Store) MailRoot() string { return s.root }

// AccountsDB returns the path of Accounts4.sqlite.
func (s *Store) AccountsDB() string { return s.accountsDB }

// Version returns the data version directory name, e.g. "V10".
func (s *Store) Version() string { return s.version }

// IndexModTime is the newest mtime of the Envelope Index and its -wal file.
func (s *Store) IndexModTime() time.Time {
	p := indexPath(s.root, s.version)
	var latest time.Time
	for _, f := range []string{p, p + "-wal"} {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}

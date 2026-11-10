package safety

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const maxResultRunes = 200

// ActionLine is one actions.log record. It never holds subjects, addresses or
// bodies: Account is the Mail account UUID and Result a short status.
type ActionLine struct {
	TS      time.Time `json:"ts"`
	Action  string    `json:"action"`
	Account string    `json:"account"`
	Count   int       `json:"count"`
	Result  string    `json:"result"`
}

// State is mailkite's own small store: messages hidden from listings and the
// actions log. Both files are 0600 in a 0700 directory.
type State struct {
	db      *sql.DB
	logPath string
}

// OpenState opens (creating if needed) dir/state.sqlite and dir/actions.log.
func OpenState(dir string) (*State, error) {
	if err := mkdirPrivate(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "state.sqlite")
	// Create the file 0600 first: SQLite gives its journal files the same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	u := url.URL{Path: path}
	db, err := sql.Open("sqlite", "file:"+u.EscapedPath()+"?_txlock=immediate&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := initHidden(db); err != nil {
		db.Close()
		return nil, err
	}
	return &State{db: db, logPath: filepath.Join(dir, "actions.log")}, nil
}

// initHidden creates the hidden table keyed by (account, rowid): the Mail account
// UUID and the ROWID of the message in Mail's index. It is only a view cache, so an
// older table (keyed by Message-ID, or without the account) is dropped.
func initHidden(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('hidden') WHERE name IN ('account', 'rowid')`).Scan(&n); err != nil {
		return err
	}
	if n != 2 {
		if _, err := db.Exec(`DROP TABLE IF EXISTS hidden`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS hidden(account TEXT NOT NULL, rowid INTEGER NOT NULL, at INTEGER,
		PRIMARY KEY(account, rowid))`)
	return err
}

// HiddenKey is one hidden message: the Mail account UUID and the message's ROWID
// in Mail's index. Copies of the same Message-ID are separate rows, so hiding one
// never hides another.
type HiddenKey struct {
	Account string
	RowID   int64
}

// Close closes the database.
func (s *State) Close() error { return s.db.Close() }

func (s *State) each(query, account string, rowids []int64, args func(id int64) []any) error {
	if strings.TrimSpace(account) == "" {
		return errors.New("hidden: empty account")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(query)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, id := range rowids {
		if id <= 0 {
			continue
		}
		if _, err := st.Exec(args(id)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Hide records ROWIDs of one Mail account (UUID) as hidden at time at. Zero and
// negative ROWIDs are skipped.
func (s *State) Hide(account string, rowids []int64, at time.Time) error {
	return s.each(`INSERT OR REPLACE INTO hidden(account, rowid, at) VALUES(?, ?, ?)`, account, rowids,
		func(id int64) []any { return []any{account, id, at.Unix()} })
}

// Unhide forgets hidden ROWIDs of one account.
func (s *State) Unhide(account string, rowids []int64) error {
	return s.each(`DELETE FROM hidden WHERE account = ? AND rowid = ?`, account, rowids,
		func(id int64) []any { return []any{account, id} })
}

// Hidden returns the set of hidden (account, ROWID) pairs.
func (s *State) Hidden(ctx context.Context) (map[HiddenKey]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, rowid FROM hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[HiddenKey]struct{}{}
	for rows.Next() {
		var k HiddenKey
		if err := rows.Scan(&k.Account, &k.RowID); err != nil {
			return nil, err
		}
		out[k] = struct{}{}
	}
	return out, rows.Err()
}

// Log appends l to actions.log as one JSON line. An Account that looks like an
// address is refused: the log must never contain addresses.
func (s *State) Log(l ActionLine) error {
	if strings.Contains(l.Account, "@") {
		return errors.New("actions log: account must be a Mail account UUID, not an address")
	}
	l.Result = truncateRunes(clean(l.Result, false), maxResultRunes)
	l.Action = clean(l.Action, false)
	l.TS = l.TS.Truncate(time.Second)
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

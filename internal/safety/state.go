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

// initHidden creates the hidden table keyed by (account, message_id). An older
// table without the account column is only a view cache: it is dropped.
func initHidden(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('hidden') WHERE name = 'account'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`DROP TABLE IF EXISTS hidden`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS hidden(account TEXT NOT NULL, message_id TEXT NOT NULL, at INTEGER,
		PRIMARY KEY(account, message_id))`)
	return err
}

// HiddenKey is one hidden message: the Mail account UUID and the Message-ID.
type HiddenKey struct{ Account, MessageID string }

// Close closes the database.
func (s *State) Close() error { return s.db.Close() }

func (s *State) each(query, account string, ids []string, args func(id string) []any) error {
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
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, err := st.Exec(args(id)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Hide records Message-IDs of one Mail account (UUID) as hidden at time at.
// Empty IDs are skipped.
func (s *State) Hide(account string, messageIDs []string, at time.Time) error {
	return s.each(`INSERT OR REPLACE INTO hidden(account, message_id, at) VALUES(?, ?, ?)`, account, messageIDs,
		func(id string) []any { return []any{account, id, at.Unix()} })
}

// Unhide forgets hidden Message-IDs of one account.
func (s *State) Unhide(account string, messageIDs []string) error {
	return s.each(`DELETE FROM hidden WHERE account = ? AND message_id = ?`, account, messageIDs,
		func(id string) []any { return []any{account, id} })
}

// Hidden returns the set of hidden (account, Message-ID) pairs.
func (s *State) Hidden(ctx context.Context) (map[HiddenKey]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, message_id FROM hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[HiddenKey]struct{}{}
	for rows.Next() {
		var k HiddenKey
		if err := rows.Scan(&k.Account, &k.MessageID); err != nil {
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

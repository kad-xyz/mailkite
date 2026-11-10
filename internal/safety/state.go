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
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS hidden(message_id TEXT PRIMARY KEY, at INTEGER)`); err != nil {
		db.Close()
		return nil, err
	}
	return &State{db: db, logPath: filepath.Join(dir, "actions.log")}, nil
}

// Close closes the database.
func (s *State) Close() error { return s.db.Close() }

func (s *State) each(query string, ids []string, args func(id string) []any) error {
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

// Hide records Message-IDs as hidden at time at. Empty IDs are skipped.
func (s *State) Hide(messageIDs []string, at time.Time) error {
	return s.each(`INSERT OR REPLACE INTO hidden(message_id, at) VALUES(?, ?)`, messageIDs,
		func(id string) []any { return []any{id, at.Unix()} })
}

// Unhide forgets hidden Message-IDs.
func (s *State) Unhide(messageIDs []string) error {
	return s.each(`DELETE FROM hidden WHERE message_id = ?`, messageIDs,
		func(id string) []any { return []any{id} })
}

// Hidden returns the set of hidden Message-IDs.
func (s *State) Hidden(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id FROM hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
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

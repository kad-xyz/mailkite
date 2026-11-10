// Package fts is a lazy, incremental SQLite FTS5 index over Mail message text.
package fts

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/message"

	_ "modernc.org/sqlite"
)

const (
	pruneEvery   = 60 * time.Second
	maxBodyBytes = 256 << 10
	defaultLimit = 500
)

// Index is the full-text index stored in our own writable SQLite file.
type Index struct {
	db *sql.DB
}

// DefaultPath is ~/Library/Caches/mailkite/index.sqlite.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Caches", "mailkite", "index.sqlite"), nil
}

// Open opens (creating if needed) the index at path; an empty path means DefaultPath.
func Open(path string) (*Index, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	u := url.URL{Path: path}
	db, err := sql.Open("sqlite", "file:"+u.EscapedPath()+"?_pragma=busy_timeout(2000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(subject, sender, recipients, body, attachments, tokenize='unicode61 remove_diacritics 2');
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init index: %w", err)
	}
	return &Index{db: db}, nil
}

// Close closes the index.
func (ix *Index) Close() error { return ix.db.Close() }

var yo = strings.NewReplacer("ё", "е", "Ё", "Е")

func norm(s string) string { return yo.Replace(s) }

func (ix *Index) metaInt(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) int64 {
	var v any
	if err := q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&v); err != nil {
		return 0
	}
	switch x := v.(type) {
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	}
	return 0
}

// Sync indexes messages newer than meta.last_rowid and, at most once a minute, drops
// index entries whose messages no longer exist.
func (ix *Index) Sync(ctx context.Context, st *maildata.Store) (added, removed int, err error) {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	last := ix.metaInt(ctx, tx, "last_rowid")
	ins, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO docs(rowid, subject, sender, recipients, body, attachments) VALUES(?,?,?,?,?,?)")
	if err != nil {
		return 0, 0, err
	}
	defer ins.Close()
	maxID := last
	err = st.ScanSince(ctx, last, func(s maildata.Summary, mb maildata.Mailbox, to, att []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		body := ""
		if p, _, ok := st.EmlxPath(s.ID, mb); ok {
			if raw, e := message.ReadEmlx(p); e == nil {
				if m, e := message.Parse(raw); e == nil {
					body = m.Text
					for _, part := range m.Parts {
						if part.Name != "" {
							att = append(att, part.Name)
						}
					}
				}
			}
		}
		if len(body) > maxBodyBytes {
			body = strings.ToValidUTF8(body[:maxBodyBytes], "")
		}
		sender := strings.TrimSpace(s.FromName + " " + s.From)
		_, e := ins.ExecContext(ctx, s.ID, norm(s.Subject), norm(sender), norm(strings.Join(to, " ")), norm(body), norm(strings.Join(att, " ")))
		if e != nil {
			return e
		}
		added++
		if s.ID > maxID {
			maxID = s.ID
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	if maxID > last {
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key,value) VALUES('last_rowid',?)", maxID); err != nil {
			return 0, 0, err
		}
	}
	now := time.Now()
	if now.Sub(time.Unix(ix.metaInt(ctx, tx, "last_prune"), 0)) >= pruneEvery {
		removed, err = ix.prune(ctx, tx, st)
		if err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key,value) VALUES('last_prune',?)", now.Unix()); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return added, removed, nil
}

func (ix *Index) prune(ctx context.Context, tx *sql.Tx, st *maildata.Store) (int, error) {
	live, err := st.LiveIDs(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT rowid FROM docs")
	if err != nil {
		return 0, err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := live[id]; !ok {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range gone {
		if _, err := tx.ExecContext(ctx, "DELETE FROM docs WHERE rowid=?", id); err != nil {
			return 0, err
		}
	}
	return len(gone), nil
}

// Rebuild drops everything and re-indexes from scratch.
func (ix *Index) Rebuild(ctx context.Context, st *maildata.Store) error {
	if _, err := ix.db.ExecContext(ctx, "DELETE FROM docs; DELETE FROM meta;"); err != nil {
		return err
	}
	_, _, err := ix.Sync(ctx, st)
	return err
}

// escapeQuery turns free text into an implicit-AND prefix query of quoted terms.
func escapeQuery(q string) string {
	var terms []string
	for _, w := range strings.Fields(norm(q)) {
		if !strings.ContainsFunc(w, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			continue // nothing the tokenizer would keep
		}
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}

// Search returns message ROWIDs ordered by bm25, best first. limit <= 0 means 500.
func (ix *Index) Search(ctx context.Context, query string, limit int) ([]int64, error) {
	q := escapeQuery(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	rows, err := ix.db.QueryContext(ctx, "SELECT rowid FROM docs WHERE docs MATCH ? ORDER BY bm25(docs), rowid DESC LIMIT ?", q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

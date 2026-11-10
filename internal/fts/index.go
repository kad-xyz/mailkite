// Package fts is a lazy, incremental SQLite FTS5 index over Mail message text.
package fts

import (
	"context"
	"database/sql"
	"errors"
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
	pendingBatch = 200
)

// chunkSize is how many messages one Sync transaction covers; variables so tests can shrink them.
var chunkSize = 500

// afterChunk is a test hook called after each committed chunk.
var afterChunk func()

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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	u := url.URL{Path: path}
	db, err := sql.Open("sqlite", "file:"+u.EscapedPath()+"?_txlock=immediate&_pragma=busy_timeout(2000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(subject, sender, recipients, body, attachments, tokenize='unicode61 remove_diacritics 2');
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value);
CREATE TABLE IF NOT EXISTS pending(rowid INTEGER PRIMARY KEY);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init index: %w", err)
	}
	for _, suf := range []string{"", "-wal", "-shm"} {
		os.Chmod(path+suf, 0o600) // the index holds mail text
	}
	return &Index{db: db}, nil
}

// Close closes the index.
func (ix *Index) Close() error { return ix.db.Close() }

var yo = strings.NewReplacer("ё", "е", "Ё", "Е")

func norm(s string) string { return yo.Replace(s) }

func metaInt(ctx context.Context, tx *sql.Tx, key string) (int64, error) {
	var v any
	if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	switch x := v.(type) {
	case int64:
		return x, nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	case []byte:
		return strconv.ParseInt(string(x), 10, 64)
	}
	return 0, nil
}

// bodyOf returns the parsed text of a message and whether it came from a complete .emlx.
func bodyOf(st *maildata.Store, id int64, mb maildata.Mailbox) (text string, parts []string, full bool) {
	p, partial, ok := st.EmlxPath(id, mb)
	if !ok {
		return "", nil, false
	}
	raw, err := message.ReadEmlx(p)
	if err != nil {
		return "", nil, false
	}
	m, err := message.Parse(raw)
	if err != nil {
		return "", nil, false
	}
	text = m.Text
	if len(text) > maxBodyBytes {
		text = strings.ToValidUTF8(text[:maxBodyBytes], "")
	}
	for _, part := range m.Parts {
		if part.Name != "" {
			parts = append(parts, part.Name)
		}
	}
	return text, parts, !partial
}

// Sync indexes messages newer than meta.last_rowid (committing every chunkSize messages),
// retries messages that were indexed without a full body, and at most once a minute drops
// index entries whose messages no longer exist.
func (ix *Index) Sync(ctx context.Context, st *maildata.Store) (added, removed int, err error) {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	last, err := metaInt(ctx, tx, "last_rowid")
	if err != nil {
		return 0, 0, err
	}
	prep := func() (*sql.Stmt, *sql.Stmt, error) {
		ins, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO docs(rowid, subject, sender, recipients, body, attachments) VALUES(?,?,?,?,?,?)")
		if err != nil {
			return nil, nil, err
		}
		pend, err := tx.PrepareContext(ctx, "INSERT OR IGNORE INTO pending(rowid) VALUES(?)")
		if err != nil {
			ins.Close()
			return nil, nil, err
		}
		return ins, pend, nil
	}
	ins, pend, err := prep()
	if err != nil {
		return 0, 0, err
	}
	maxID, inChunk := last, 0
	commit := func() error {
		ins.Close()
		pend.Close()
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key,value) VALUES('last_rowid',?)", maxID); err != nil {
			return err
		}
		return tx.Commit()
	}
	reopen := func() (err error) {
		if tx, err = ix.db.BeginTx(ctx, nil); err != nil {
			return err
		}
		ins, pend, err = prep()
		return err
	}
	err = st.ScanSince(ctx, last, func(s maildata.Summary, mb maildata.Mailbox, to, att []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, parts, full := bodyOf(st, s.ID, mb)
		att = append(att, parts...)
		sender := strings.TrimSpace(s.FromName + " " + s.From)
		if _, e := ins.ExecContext(ctx, s.ID, norm(s.Subject), norm(sender), norm(strings.Join(to, " ")), norm(body), norm(strings.Join(att, " "))); e != nil {
			return e
		}
		if !full {
			if _, e := pend.ExecContext(ctx, s.ID); e != nil {
				return e
			}
		}
		added++
		maxID = s.ID
		if inChunk++; inChunk >= chunkSize {
			inChunk = 0
			if err := commit(); err != nil {
				return err
			}
			if afterChunk != nil {
				afterChunk()
			}
			return reopen()
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	ins.Close()
	pend.Close()
	if maxID > last {
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key,value) VALUES('last_rowid',?)", maxID); err != nil {
			return 0, 0, err
		}
	}
	if err := ix.retryPending(ctx, tx, st); err != nil {
		return 0, 0, err
	}
	now := time.Now()
	lp, err := metaInt(ctx, tx, "last_prune")
	if err != nil {
		return 0, 0, err
	}
	if now.Sub(time.Unix(lp, 0)) >= pruneEvery {
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

// retryPending re-reads up to pendingBatch messages that were indexed without a full body.
func (ix *Index) retryPending(ctx context.Context, tx *sql.Tx, st *maildata.Store) error {
	rows, err := tx.QueryContext(ctx, "SELECT rowid FROM pending ORDER BY rowid LIMIT ?", pendingBatch)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		_, mb, err := st.Get(ctx, id)
		if errors.Is(err, maildata.ErrNotFound) {
			if _, err := tx.ExecContext(ctx, "DELETE FROM pending WHERE rowid=?", id); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		body, _, full := bodyOf(st, id, mb)
		if !full {
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE docs SET body=? WHERE rowid=?", norm(body), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM pending WHERE rowid=?", id); err != nil {
			return err
		}
	}
	return nil
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
		for _, q := range []string{"DELETE FROM docs WHERE rowid=?", "DELETE FROM pending WHERE rowid=?"} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return 0, err
			}
		}
	}
	return len(gone), nil
}

// Rebuild drops everything and re-indexes from scratch.
func (ix *Index) Rebuild(ctx context.Context, st *maildata.Store) error {
	if _, err := ix.db.ExecContext(ctx, "DELETE FROM docs; DELETE FROM meta; DELETE FROM pending;"); err != nil {
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

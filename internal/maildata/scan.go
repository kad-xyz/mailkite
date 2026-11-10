package maildata

import (
	"context"
	"database/sql"
	"strings"
)

const listSep = "\x1f"

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, listSep)
}

// ScanSince streams every non-deleted message with ROWID > afterRowID, in ascending ROWID order,
// across all mailboxes (trash and junk included). to holds the recipient addresses.
func (s *Store) ScanSince(ctx context.Context, afterRowID int64, fn func(sm Summary, mb Mailbox, to []string, attachNames []string) error) error {
	mbs, err := s.loadMailboxes(ctx)
	if err != nil {
		return err
	}
	names := s.accountNames()
	byID := make(map[int64]Mailbox, len(mbs))
	for _, mb := range mbs {
		byID[mb.ID] = mb
	}
	q := "SELECT " + summaryCols + `,
	coalesce((SELECT group_concat(ra.address, char(31)) FROM recipients r JOIN addresses ra ON ra.ROWID = r.address WHERE r.message = m.ROWID),''),
	coalesce((SELECT group_concat(t.name, char(31)) FROM attachments t WHERE t.message = m.ROWID AND t.name IS NOT NULL),'')` + summaryFrom + `
WHERE m.deleted = 0 AND m.ROWID > ? ORDER BY m.ROWID ASC`
	rows, err := s.db.QueryContext(ctx, q, afterRowID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var to, att string
		sm, mid, err := scanRow(rows, &to, &att)
		if err != nil {
			return err
		}
		mb := byID[mid]
		sm.Account, sm.Mailbox, sm.AccountUUID = accountName(names, mb.AccountUUID), mb.Name, mb.AccountUUID
		if err := fn(sm, mb, splitList(to), splitList(att)); err != nil {
			return err
		}
	}
	return rows.Err()
}

// LiveIDs returns the ROWIDs of all non-deleted messages.
func (s *Store) LiveIDs(ctx context.Context) (map[int64]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT ROWID FROM messages WHERE deleted = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// PresentRows returns which of the ROWIDs are still non-deleted messages of Mail's
// index. It looks up only the given ROWIDs.
func (s *Store) PresentRows(ctx context.Context, rowids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	const batch = 400
	for i := 0; i < len(rowids); i += batch {
		chunk := rowids[i:min(i+batch, len(rowids))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT ROWID FROM messages WHERE deleted = 0 AND ROWID IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// Copy is one row of a message in Mail's index: its ROWID and mailbox name.
type Copy struct {
	ID      int64
	Mailbox string
}

// Copies returns every non-deleted row of the account (Mail UUID) whose
// Message-ID header is messageID (as stored, with angle brackets), in any mailbox.
func (s *Store) Copies(ctx context.Context, account, messageID string) ([]Copy, error) {
	if messageID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.ROWID, b.ROWID, b.url FROM messages m
		JOIN message_global_data g ON g.ROWID = m.global_message_id
		JOIN mailboxes b ON b.ROWID = m.mailbox
		WHERE m.deleted = 0 AND g.message_id_header = ? ORDER BY m.ROWID`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Copy
	for rows.Next() {
		var id, mbID int64
		var u sql.NullString
		if err := rows.Scan(&id, &mbID, &u); err != nil {
			return nil, err
		}
		if mb := ParseMailboxURL(mbID, u.String); mb.AccountUUID == account {
			out = append(out, Copy{ID: id, Mailbox: mb.Name})
		}
	}
	return out, rows.Err()
}

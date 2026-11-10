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

// MsgKey identifies a message the way Mail's index does: Mail account UUID and
// Message-ID header (as stored, with angle brackets).
type MsgKey struct{ Account, MessageID string }

// PresentKeys returns which of keys still have a non-deleted message in the index,
// in any mailbox of that account (Trash included). It looks up only the given
// Message-IDs, not the whole index.
func (s *Store) PresentKeys(ctx context.Context, keys []MsgKey) (map[MsgKey]bool, error) {
	out := map[MsgKey]bool{}
	const batch = 400
	for i := 0; i < len(keys); i += batch {
		chunk := keys[i:min(i+batch, len(keys))]
		args := make([]any, len(chunk))
		for j, k := range chunk {
			args[j] = k.MessageID
		}
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT g.message_id_header, b.url FROM messages m
			JOIN message_global_data g ON g.ROWID = m.global_message_id
			JOIN mailboxes b ON b.ROWID = m.mailbox
			WHERE m.deleted = 0 AND g.message_id_header IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, u sql.NullString
			if err := rows.Scan(&id, &u); err != nil {
				rows.Close()
				return nil, err
			}
			out[MsgKey{Account: ParseMailboxURL(0, u.String).AccountUUID, MessageID: id.String}] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

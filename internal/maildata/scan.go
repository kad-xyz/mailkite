package maildata

import (
	"context"
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

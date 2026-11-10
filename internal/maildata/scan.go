package maildata

import (
	"context"
	"strings"
	"time"
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
	q := `SELECT m.ROWID, coalesce(g.message_id_header,''), coalesce(m.date_received,0), coalesce(a.address,''), coalesce(a.comment,''),
	coalesce(s.subject,''), m.mailbox, m.read, m.flagged, coalesce(m.size,0),
	(SELECT count(*) FROM attachments t WHERE t.message = m.ROWID), m.list_id_hash IS NOT NULL, coalesce(m.unsubscribe_type,0),
	coalesce((SELECT group_concat(ra.address, char(31)) FROM recipients r JOIN addresses ra ON ra.ROWID = r.address WHERE r.message = m.ROWID),''),
	coalesce((SELECT group_concat(t.name, char(31)) FROM attachments t WHERE t.message = m.ROWID AND t.name IS NOT NULL),'')
FROM messages m
LEFT JOIN addresses a ON a.ROWID = m.sender
LEFT JOIN subjects s ON s.ROWID = m.subject
LEFT JOIN message_global_data g ON g.ROWID = m.global_message_id
WHERE m.deleted = 0 AND m.ROWID > ? ORDER BY m.ROWID ASC`
	rows, err := s.db.QueryContext(ctx, q, afterRowID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sm Summary
		var date, mid int64
		var read, flagged, listID bool
		var to, att string
		if err := rows.Scan(&sm.ID, &sm.MessageID, &date, &sm.From, &sm.FromName, &sm.Subject, &mid, &read, &flagged, &sm.Size, &sm.Attachments, &listID, &sm.Unsub, &to, &att); err != nil {
			return err
		}
		sm.Date = time.Unix(date, 0)
		sm.Read, sm.Flagged, sm.ListID = read, flagged, listID
		mb := byID[mid]
		sm.Account, sm.Mailbox = accountName(names, mb.AccountUUID), mb.Name
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

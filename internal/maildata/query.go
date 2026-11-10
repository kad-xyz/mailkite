package maildata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned by Get for an unknown message id.
var ErrNotFound = errors.New("message not found")

// Account is one mail account that owns at least one mailbox.
type Account struct {
	UUID, Name, Kind string
	Mailboxes        []Mailbox
	Messages, Unread int // non-deleted messages outside trash and junk mailboxes
}

// Filter selects messages. Zero values mean "no constraint".
type Filter struct {
	Account, Mailbox string // account UUID or name; mailbox name (case-insensitive)
	Role             Role
	From, Subject    string // case-insensitive substrings
	Since, Until     time.Time
	Unread, Flagged  *bool
	HasAttachments   *bool
	IDs              []int64
	Limit, Offset    int
}

// Summary is the index-level description of one message.
type Summary struct {
	ID               int64
	MessageID        string
	Date             time.Time
	From, FromName   string
	Subject          string
	Account, Mailbox string // account NAME (resolved) and mailbox name
	AccountUUID      string // Mail account UUID
	Read, Flagged    bool
	Size             int64
	Attachments      int
	ListID           bool
	Unsub            int
}

// SenderGroup aggregates messages by sender address.
type SenderGroup struct {
	Address, Name string
	Count, Unread int
	Last          time.Time
	ListID        bool
	Unsub         int
}

const (
	defaultLimit = 20
	maxLimit     = 200
)

func (f Filter) limit() int {
	switch {
	case f.Limit <= 0:
		return defaultLimit
	case f.Limit > maxLimit:
		return maxLimit
	}
	return f.Limit
}

func (f Filter) offset() int {
	if f.Offset < 0 {
		return 0
	}
	return f.Offset
}

// accountNames returns UUID -> resolved account name, cached until the Accounts db changes.
func (s *Store) accountNames() map[string]string {
	s.accMu.Lock()
	defer s.accMu.Unlock()
	stamp := ""
	for _, suf := range []string{"", "-wal"} {
		if st, err := os.Stat(s.accountsDB + suf); err == nil {
			stamp += fmt.Sprintf("%d:%d;", st.ModTime().UnixNano(), st.Size())
		}
	}
	if s.accNames != nil && stamp == s.accStamp {
		return s.accNames
	}
	names, err := s.loadAccountNames()
	if err != nil {
		// Do not cache a failure: fall back to UUIDs for this call and retry next time.
		return map[string]string{}
	}
	s.accNames, s.accStamp = names, stamp
	return names
}

func (s *Store) loadAccountNames() (map[string]string, error) {
	db, err := openReadOnly(s.accountsDB)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT a.ZIDENTIFIER, a.ZACCOUNTDESCRIPTION, a.ZUSERNAME, p.ZACCOUNTDESCRIPTION, p.ZUSERNAME, t.ZACCOUNTTYPEDESCRIPTION
		FROM ZACCOUNT a LEFT JOIN ZACCOUNT p ON p.Z_PK = a.ZPARENTACCOUNT
		LEFT JOIN ZACCOUNTTYPE t ON t.Z_PK = a.ZACCOUNTTYPE WHERE a.ZIDENTIFIER IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var id string
		var c [5]sql.NullString
		if err := rows.Scan(&id, &c[0], &c[1], &c[2], &c[3], &c[4]); err != nil {
			return nil, err
		}
		for _, v := range c { // own desc, own user, parent desc, parent user, type
			if v.Valid && strings.TrimSpace(v.String) != "" {
				names[id] = v.String
				break
			}
		}
	}
	return names, rows.Err()
}

func (s *Store) loadMailboxes(ctx context.Context) ([]Mailbox, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT ROWID, url FROM mailboxes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mailbox
	for rows.Next() {
		var id int64
		var u sql.NullString
		if err := rows.Scan(&id, &u); err != nil {
			return nil, err
		}
		if u.Valid {
			out = append(out, ParseMailboxURL(id, u.String))
		}
	}
	return out, rows.Err()
}

func accountName(names map[string]string, uuid string) string {
	if n := names[uuid]; n != "" {
		return n
	}
	return uuid
}

// Accounts lists the accounts that have mailboxes, with message counts.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	mbs, err := s.loadMailboxes(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT mailbox, count(*), coalesce(sum(read=0),0) FROM messages WHERE deleted=0 GROUP BY mailbox")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type cnt struct{ n, unread int }
	counts := map[int64]cnt{}
	for rows.Next() {
		var id int64
		var c cnt
		if err := rows.Scan(&id, &c.n, &c.unread); err != nil {
			return nil, err
		}
		counts[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := s.accountNames()
	byUUID := map[string]*Account{}
	var order []string
	for _, mb := range mbs {
		a := byUUID[mb.AccountUUID]
		if a == nil {
			a = &Account{UUID: mb.AccountUUID, Name: accountName(names, mb.AccountUUID), Kind: mb.Kind}
			byUUID[mb.AccountUUID] = a
			order = append(order, mb.AccountUUID)
		}
		a.Mailboxes = append(a.Mailboxes, mb)
		// All Mail repeats every other mailbox of a Gmail account; skip it to avoid double counts
		if mb.Role != RoleTrash && mb.Role != RoleJunk && mb.Role != RoleAll {
			c := counts[mb.ID]
			a.Messages += c.n
			a.Unread += c.unread
		}
	}
	sort.Strings(order)
	out := make([]Account, 0, len(order))
	for _, u := range order {
		out = append(out, *byUUID[u])
	}
	return out, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

const summaryCols = `m.ROWID, coalesce(g.message_id_header,''), coalesce(m.date_received,0), coalesce(a.address,''), coalesce(a.comment,''),
	coalesce(s.subject,''), m.mailbox, m.read, m.flagged, coalesce(m.size,0),
	(SELECT count(*) FROM attachments t WHERE t.message = m.ROWID), m.list_id_hash IS NOT NULL, coalesce(m.unsubscribe_type,0)`

const summaryFrom = ` FROM messages m
LEFT JOIN addresses a ON a.ROWID = m.sender
LEFT JOIN subjects s ON s.ROWID = m.subject
LEFT JOIN message_global_data g ON g.ROWID = m.global_message_id`

const selectSummary = "SELECT " + summaryCols + summaryFrom

// scanRow reads one selectSummary row (plus any extra trailing columns); the mailbox is returned as its ROWID.
func scanRow(sc interface{ Scan(...any) error }, extra ...any) (sm Summary, mbox int64, err error) {
	var date int64
	var read, flagged, listID bool
	dest := append([]any{&sm.ID, &sm.MessageID, &date, &sm.From, &sm.FromName, &sm.Subject, &mbox, &read, &flagged, &sm.Size, &sm.Attachments, &listID, &sm.Unsub}, extra...)
	err = sc.Scan(dest...)
	sm.Date = time.Unix(date, 0)
	sm.Read, sm.Flagged, sm.ListID = read, flagged, listID
	return
}

// scan streams the messages matching f (without paging) in date order; fn returns false to stop.
// Dedup is left to the callers.
func (s *Store) scan(ctx context.Context, f Filter, fn func(sm Summary, mb Mailbox) bool) error {
	mbs, err := s.loadMailboxes(ctx)
	if err != nil {
		return err
	}
	names := s.accountNames()
	byID := make(map[int64]Mailbox, len(mbs))
	var allowed []any
	for _, mb := range mbs {
		byID[mb.ID] = mb
		switch {
		case f.Role != "" && mb.Role != f.Role:
			continue
		case f.Role == "" && f.Mailbox == "" && (mb.Role == RoleTrash || mb.Role == RoleJunk):
			continue
		case f.Mailbox != "" && !strings.EqualFold(mb.Name, f.Mailbox):
			continue
		case f.Account != "" && !strings.EqualFold(mb.AccountUUID, f.Account) && !strings.EqualFold(accountName(names, mb.AccountUUID), f.Account):
			continue
		}
		allowed = append(allowed, mb.ID)
	}
	if len(allowed) == 0 {
		return nil
	}
	where := []string{"m.deleted = 0", "m.mailbox IN (?" + strings.Repeat(",?", len(allowed)-1) + ")"}
	args := allowed
	var goFilters []func(Summary) bool
	text := func(needle, col string, get func(Summary) string) {
		if needle == "" {
			return
		}
		if isASCII(needle) { // SQLite LIKE folds ASCII only
			where = append(where, col+" LIKE ? ESCAPE '\\'")
			args = append(args, "%"+likeEscape(needle)+"%")
			return
		}
		low := strings.ToLower(needle)
		goFilters = append(goFilters, func(sm Summary) bool { return strings.Contains(strings.ToLower(get(sm)), low) })
	}
	if f.From != "" && isASCII(f.From) {
		where = append(where, "(a.address LIKE ? ESCAPE '\\' OR a.comment LIKE ? ESCAPE '\\')")
		p := "%" + likeEscape(f.From) + "%"
		args = append(args, p, p)
	} else if f.From != "" {
		low := strings.ToLower(f.From)
		goFilters = append(goFilters, func(sm Summary) bool {
			return strings.Contains(strings.ToLower(sm.From), low) || strings.Contains(strings.ToLower(sm.FromName), low)
		})
	}
	text(f.Subject, "s.subject", func(sm Summary) string { return sm.Subject })
	if !f.Since.IsZero() {
		where = append(where, "m.date_received >= ?")
		args = append(args, f.Since.Unix())
	}
	if !f.Until.IsZero() {
		where = append(where, "m.date_received < ?")
		args = append(args, f.Until.Unix())
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	if f.Unread != nil {
		where = append(where, "m.read = ?")
		args = append(args, b2i(!*f.Unread))
	}
	if f.Flagged != nil {
		where = append(where, "m.flagged = ?")
		args = append(args, b2i(*f.Flagged))
	}
	if f.HasAttachments != nil {
		not := ""
		if !*f.HasAttachments {
			not = "NOT "
		}
		where = append(where, not+"EXISTS (SELECT 1 FROM attachments t WHERE t.message = m.ROWID)")
	}
	if len(f.IDs) > 0 {
		where = append(where, "m.ROWID IN (?"+strings.Repeat(",?", len(f.IDs)-1)+")")
		for _, id := range f.IDs {
			args = append(args, id)
		}
	}
	q := selectSummary + " WHERE " + strings.Join(where, " AND ") + " ORDER BY m.date_received DESC, m.ROWID DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
rowLoop:
	for rows.Next() {
		sm, mid, err := scanRow(rows)
		if err != nil {
			return err
		}
		for _, g := range goFilters {
			if !g(sm) {
				continue rowLoop
			}
		}
		mb := byID[mid]
		sm.Account, sm.Mailbox, sm.AccountUUID = accountName(names, mb.AccountUUID), mb.Name, mb.AccountUUID
		if !fn(sm, mb) {
			break
		}
	}
	return rows.Err()
}

// dedupKey collapses copies of one message within an account (Gmail's INBOX and
// All Mail); the same Message-ID in two accounts is two messages.
func dedupKey(sm Summary, mb Mailbox) string {
	if sm.MessageID != "" {
		return "m:" + mb.AccountUUID + "\x00" + sm.MessageID
	}
	return fmt.Sprintf("r:%d", sm.ID)
}

// Find returns one page of messages, newest first, and whether more exist.
func (s *Store) Find(ctx context.Context, f Filter) (items []Summary, more bool, err error) {
	need := f.offset() + f.limit() + 1
	type entry struct {
		sm    Summary
		isAll bool
	}
	var uniq []entry
	pos := map[string]int{}
	err = s.scan(ctx, f, func(sm Summary, mb Mailbox) bool {
		k := dedupKey(sm, mb)
		if i, ok := pos[k]; ok {
			if uniq[i].isAll && mb.Role != RoleAll {
				uniq[i] = entry{sm, false}
			}
			return true
		}
		if len(uniq) >= need {
			return false
		}
		pos[k] = len(uniq)
		uniq = append(uniq, entry{sm, mb.Role == RoleAll})
		return true
	})
	if err != nil {
		return nil, false, err
	}
	if f.offset() >= len(uniq) {
		return nil, false, nil
	}
	end := f.offset() + f.limit()
	if end < len(uniq) {
		more = true
	} else {
		end = len(uniq)
	}
	for _, e := range uniq[f.offset():end] {
		items = append(items, e.sm)
	}
	return items, more, nil
}

// Get returns one message by ROWID together with its mailbox.
func (s *Store) Get(ctx context.Context, id int64) (Summary, Mailbox, error) {
	sm, mid, err := scanRow(s.db.QueryRowContext(ctx, selectSummary+" WHERE m.ROWID = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Summary{}, Mailbox{}, ErrNotFound
	}
	if err != nil {
		return Summary{}, Mailbox{}, err
	}
	var u sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT url FROM mailboxes WHERE ROWID = ?", mid).Scan(&u); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Summary{}, Mailbox{}, err
	}
	mb := ParseMailboxURL(mid, u.String)
	sm.Account, sm.Mailbox, sm.AccountUUID = accountName(s.accountNames(), mb.AccountUUID), mb.Name, mb.AccountUUID
	return sm, mb, nil
}

// Senders groups the matching messages (after dedup) by sender address, most frequent first.
func (s *Store) Senders(ctx context.Context, f Filter, minCount int) ([]SenderGroup, error) {
	seen := map[string]bool{}
	idx := map[string]int{}
	var groups []SenderGroup
	byDefault := f.Role == "" && f.Mailbox == ""
	err := s.scan(ctx, f, func(sm Summary, mb Mailbox) bool {
		if byDefault && (mb.Role == RoleSent || mb.Role == RoleDrafts) {
			return true // our own addresses, not senders
		}
		k := dedupKey(sm, mb)
		if seen[k] {
			return true
		}
		seen[k] = true
		a := strings.ToLower(sm.From)
		i, ok := idx[a]
		if !ok {
			i = len(groups)
			idx[a] = i
			groups = append(groups, SenderGroup{Address: sm.From, Name: sm.FromName, Last: sm.Date})
		}
		g := &groups[i]
		g.Count++
		if !sm.Read {
			g.Unread++
		}
		g.ListID = g.ListID || sm.ListID
		if sm.Unsub > g.Unsub {
			g.Unsub = sm.Unsub
		}
		if g.Name == "" {
			g.Name = sm.FromName
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	out := groups[:0]
	for _, g := range groups {
		if g.Count >= minCount {
			out = append(out, g)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}

// RemoteID returns messages.remote_id of a message: its IMAP UID in its mailbox
// (0 when Mail does not know it). It is only a hint; the server is the authority.
func (s *Store) RemoteID(ctx context.Context, id int64) (uint32, error) {
	var uid sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT remote_id FROM messages WHERE ROWID = ?", id).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if !uid.Valid || uid.Int64 <= 0 || uid.Int64 > 0xFFFFFFFF {
		return 0, nil
	}
	return uint32(uid.Int64), nil
}

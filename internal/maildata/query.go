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
	Email            string // login address from the Accounts db, empty when unknown
	Mailboxes        []Mailbox
	Messages, Unread int // non-deleted messages outside trash and junk, each counted once
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
	// Skip, when set, drops matching messages before dedup and paging (it sees
	// Summary.ID and AccountUUID); used to hide erased messages by ROWID.
	Skip func(Summary) bool
	// DedupKey, when set, replaces the key that collapses copies of one message
	// (default: account and Message-ID). Copies with different keys are listed
	// separately.
	DedupKey func(Summary, Mailbox) string
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
	names, emails, err := s.loadAccountNames()
	if err != nil {
		// Do not cache a failure: fall back to UUIDs for this call and retry next time.
		return map[string]string{}
	}
	s.accNames, s.accEmail, s.accStamp = names, emails, stamp
	return names
}

// accountEmails returns UUID -> login address of the account (own or parent), when it looks like an email.
func (s *Store) accountEmails() map[string]string {
	s.accountNames()
	s.accMu.Lock()
	defer s.accMu.Unlock()
	return s.accEmail
}

func (s *Store) loadAccountNames() (map[string]string, map[string]string, error) {
	db, err := openReadOnly(s.accountsDB)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT a.ZIDENTIFIER, a.ZACCOUNTDESCRIPTION, a.ZUSERNAME, p.ZACCOUNTDESCRIPTION, p.ZUSERNAME, t.ZACCOUNTTYPEDESCRIPTION
		FROM ZACCOUNT a LEFT JOIN ZACCOUNT p ON p.Z_PK = a.ZPARENTACCOUNT
		LEFT JOIN ZACCOUNTTYPE t ON t.Z_PK = a.ZACCOUNTTYPE WHERE a.ZIDENTIFIER IS NOT NULL`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	names, emails := map[string]string{}, map[string]string{}
	for rows.Next() {
		var id string
		var c [5]sql.NullString
		if err := rows.Scan(&id, &c[0], &c[1], &c[2], &c[3], &c[4]); err != nil {
			return nil, nil, err
		}
		for _, v := range []sql.NullString{c[1], c[3]} { // own user, parent user
			if v.Valid && strings.Contains(v.String, "@") {
				emails[id] = strings.TrimSpace(v.String)
				break
			}
		}
		for _, v := range c { // own desc, own user, parent desc, parent user, type
			if v.Valid && strings.TrimSpace(v.String) != "" {
				names[id] = v.String
				break
			}
		}
	}
	return names, emails, rows.Err()
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
	names, emails := s.accountNames(), s.accountEmails()
	byUUID := map[string]*Account{}
	byID := make(map[int64]Mailbox, len(mbs))
	var order []string
	for _, mb := range mbs {
		byID[mb.ID] = mb
		a := byUUID[mb.AccountUUID]
		if a == nil {
			a = &Account{UUID: mb.AccountUUID, Name: accountName(names, mb.AccountUUID), Email: emails[mb.AccountUUID], Kind: mb.Kind}
			byUUID[mb.AccountUUID] = a
			order = append(order, mb.AccountUUID)
		}
		a.Mailboxes = append(a.Mailboxes, mb)
	}
	// Count each message once per account. Mail stores a Gmail message once (in All
	// Mail, with INBOX and the like as labels), but an index may also keep a copy
	// per mailbox; copies share global_message_id, one row per Message-ID.
	rows, err := s.db.QueryContext(ctx, "SELECT ROWID, mailbox, global_message_id, read FROM messages WHERE deleted = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		account string
		global  int64
	}
	seen := map[key]bool{}
	for rows.Next() {
		var id, mid int64
		var global sql.NullInt64
		var read bool
		if err := rows.Scan(&id, &mid, &global, &read); err != nil {
			return nil, err
		}
		mb, ok := byID[mid]
		if !ok || mb.Role == RoleTrash || mb.Role == RoleJunk {
			continue
		}
		k := key{mb.AccountUUID, -id} // no global row: the copy counts on its own
		if global.Valid && global.Int64 > 0 {
			k.global = global.Int64
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		a := byUUID[mb.AccountUUID]
		a.Messages++
		if !read {
			a.Unread++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
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
	var allowed, stored []any
	for _, mb := range mbs {
		byID[mb.ID] = mb
		if f.Account != "" && !strings.EqualFold(mb.AccountUUID, f.Account) && !strings.EqualFold(accountName(names, mb.AccountUUID), f.Account) {
			continue
		}
		if mb.Role != RoleTrash && mb.Role != RoleJunk {
			stored = append(stored, mb.ID)
		}
		switch {
		case f.Role != "" && mb.Role != f.Role:
			continue
		case f.Role == "" && f.Mailbox == "" && (mb.Role == RoleTrash || mb.Role == RoleJunk):
			continue
		case f.Mailbox != "" && !strings.EqualFold(mb.Name, f.Mailbox):
			continue
		}
		allowed = append(allowed, mb.ID)
	}
	if len(allowed) == 0 {
		return nil
	}
	in := func(ids []any) string { return "(?" + strings.Repeat(",?", len(ids)-1) + ")" }
	where := []string{"m.deleted = 0", "m.mailbox IN " + in(allowed)}
	args := append([]any{}, allowed...)
	// A Gmail message is stored once, in All Mail; INBOX, Sent Mail and other labels
	// are rows of the labels table. A mailbox filter also matches those labels, but
	// only for messages not stored in trash or junk.
	if s.hasLabels && (f.Role != "" || f.Mailbox != "") && len(stored) > 0 {
		where[1] = "(m.mailbox IN " + in(allowed) + " OR (m.mailbox IN " + in(stored) +
			" AND EXISTS (SELECT 1 FROM labels l WHERE l.message_id = m.ROWID AND l.mailbox_id IN " + in(allowed) + ")))"
		args = append(append(args, stored...), allowed...)
	}
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
		if f.Skip != nil && f.Skip(sm) {
			continue
		}
		if !fn(sm, mb) {
			break
		}
	}
	return rows.Err()
}

// DedupKey collapses copies of one message within an account (Gmail's INBOX and
// All Mail); the same Message-ID in two accounts is two messages.
func DedupKey(sm Summary, mb Mailbox) string {
	if sm.MessageID != "" {
		return "m:" + mb.AccountUUID + "\x00" + sm.MessageID
	}
	return fmt.Sprintf("r:%d", sm.ID)
}

// MailboxDedupKey collapses copies only within one mailbox of an account: copies
// in two mailboxes are two messages.
func MailboxDedupKey(sm Summary, mb Mailbox) string {
	if sm.MessageID != "" {
		return "m:" + mb.AccountUUID + "\x00" + mb.Name + "\x00" + sm.MessageID
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
	key := f.DedupKey
	if key == nil {
		key = DedupKey
	}
	err = s.scan(ctx, f, func(sm Summary, mb Mailbox) bool {
		k := key(sm, mb)
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
	var own map[int64]bool
	if byDefault {
		var err error
		if own, err = s.labeled(ctx, RoleSent, RoleDrafts); err != nil {
			return nil, err
		}
	}
	err := s.scan(ctx, f, func(sm Summary, mb Mailbox) bool {
		if byDefault && (mb.Role == RoleSent || mb.Role == RoleDrafts || own[sm.ID]) {
			return true // our own addresses, not senders
		}
		k := DedupKey(sm, mb)
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

// labeled returns the ROWIDs of messages that carry a label (Gmail) of a mailbox
// with one of the roles, such as Sent Mail for a message stored in All Mail.
func (s *Store) labeled(ctx context.Context, roles ...Role) (map[int64]bool, error) {
	if !s.hasLabels {
		return nil, nil
	}
	mbs, err := s.loadMailboxes(ctx)
	if err != nil {
		return nil, err
	}
	var ids []any
	for _, mb := range mbs {
		for _, r := range roles {
			if mb.Role == r {
				ids = append(ids, mb.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT message_id FROM labels WHERE mailbox_id IN (?"+strings.Repeat(",?", len(ids)-1)+")", ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
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

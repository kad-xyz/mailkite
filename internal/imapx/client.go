// Package imapx performs the server-side mail changes mailkite makes after the
// user confirms them: permanent erase (verified), move, flags and append.
//
// A Conn lives for one operation batch and must be closed; it keeps no timers
// and leaves no goroutines behind after Close.
package imapx

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/kad-xyz/mailkite/internal/config"
)

const (
	dialTimeout   = 15 * time.Second
	opTimeout     = 2 * time.Minute // used when the caller's ctx has no deadline
	logoutTimeout = 5 * time.Second
	eraseAttempts = 3 // the first try plus up to 2 retries
	gmailTrash    = "[Gmail]/Trash"
)

// DialOptions tune Dial for tests.
type DialOptions struct {
	// Insecure dials plain TCP without TLS. Tests only.
	Insecure bool
}

// Conn is a logged-in IMAP connection.
type Conn struct {
	nc       net.Conn
	c        *imapclient.Client
	selected string
	validity map[string]uint32 // UIDVALIDITY seen per mailbox on this Conn

	noCopyUID bool // tests: behave as if the server sent no COPYUID
}

// Dial connects with implicit TLS to a.IMAPHost:a.IMAPPort (993 by default) and logs in.
func Dial(ctx context.Context, a config.Account, password string) (*Conn, error) {
	return DialWith(ctx, a, password, DialOptions{})
}

// DialWith is Dial with options.
func DialWith(ctx context.Context, a config.Account, password string, o DialOptions) (*Conn, error) {
	if a.Kind == "pop" {
		return nil, errors.New("imap: account is POP, not IMAP")
	}
	if a.IMAPHost == "" || a.Email == "" {
		return nil, errors.New("imap: account has no IMAP host or email")
	}
	port := a.IMAPPort
	if port == 0 {
		port = 993
	}
	addr := net.JoinHostPort(a.IMAPHost, strconv.Itoa(port))
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	nd := &net.Dialer{Timeout: dialTimeout}
	var nc net.Conn
	var err error
	if o.Insecure {
		nc, err = nd.DialContext(dctx, "tcp", addr)
	} else {
		td := &tls.Dialer{NetDialer: nd, Config: &tls.Config{
			ServerName: a.IMAPHost,
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"imap"},
		}}
		nc, err = td.DialContext(dctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("imap: connect %s: %w", addr, err)
	}
	conn := &Conn{nc: nc, c: imapclient.New(nc, nil)}
	end := conn.begin(dctx)
	err = conn.c.WaitGreeting()
	if err == nil {
		err = conn.c.Login(a.Email, password).Wait()
	}
	end()
	if err != nil {
		conn.c.Close()
		return nil, fmt.Errorf("imap: login %s: %w", a.Email, err)
	}
	return conn, nil
}

// Verify checks that the account can log in with password, then logs out.
func Verify(ctx context.Context, a config.Account, password string) error {
	c, err := Dial(ctx, a, password)
	if err != nil {
		return err
	}
	return c.Close()
}

// Close logs out and closes the connection.
func (c *Conn) Close() error {
	if c == nil || c.c == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), logoutTimeout)
	end := c.begin(ctx)
	_ = c.c.Logout().Wait()
	end()
	cancel()
	err := c.c.Close()
	c.c = nil
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

// begin applies ctx's deadline (or opTimeout) to the socket and cuts it short when
// ctx is cancelled. The returned func clears the deadline again so the client's
// idle reader does not time out between operations.
func (c *Conn) begin(ctx context.Context) (end func()) {
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(opTimeout)
	}
	c.nc.SetDeadline(dl)
	stop := context.AfterFunc(ctx, func() { c.nc.SetDeadline(time.Unix(1, 0)) })
	return func() {
		if stop() {
			c.nc.SetDeadline(time.Time{})
		}
	}
}

func (c *Conn) sel(mailbox string) error {
	if c.selected == mailbox {
		return nil
	}
	return c.selFresh(mailbox)
}

// selFresh always issues SELECT and refuses when the mailbox's UIDVALIDITY differs
// from the one seen earlier on this Conn (UIDs would point at other messages).
func (c *Conn) selFresh(mailbox string) error {
	d, err := c.c.Select(mailbox, nil).Wait()
	if err != nil {
		c.selected = ""
		return fmt.Errorf("imap: select %q: %w", mailbox, err)
	}
	c.selected = mailbox
	if c.validity == nil {
		c.validity = map[string]uint32{}
	}
	if prev, ok := c.validity[mailbox]; ok && prev != d.UIDValidity {
		c.selected = ""
		return fmt.Errorf("imap: UIDVALIDITY of %q changed (%d -> %d); refusing to act on old UIDs", mailbox, prev, d.UIDValidity)
	}
	c.validity[mailbox] = d.UIDValidity
	return nil
}

// normID trims spaces and angle brackets so Message-IDs compare exactly.
func normID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.TrimSpace(id)
}

func uidSet(uids []uint32) imap.UIDSet {
	var s imap.UIDSet
	for _, u := range uids {
		s.AddNum(imap.UID(u))
	}
	return s
}

func fromUIDs(l []imap.UID) []uint32 {
	out := make([]uint32, len(l))
	for i, u := range l {
		out[i] = uint32(u)
	}
	return out
}

// envelopeIDs fetches the Message-ID of each existing UID in the selected mailbox.
func (c *Conn) envelopeIDs(uids []uint32) (map[uint32]string, error) {
	out := map[uint32]string{}
	if len(uids) == 0 {
		return out, nil
	}
	msgs, err := c.c.Fetch(uidSet(uids), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: fetch envelope: %w", err)
	}
	for _, m := range msgs {
		id := ""
		if m.Envelope != nil {
			id = normID(m.Envelope.MessageID)
		}
		out[uint32(m.UID)] = id
	}
	return out, nil
}

// searchID returns the UIDs in the selected mailbox whose Message-ID equals id exactly.
// SEARCH matches substrings, so every hit is confirmed through its envelope.
func (c *Conn) searchID(id string) ([]uint32, error) {
	id = normID(id)
	if id == "" {
		return nil, nil
	}
	for _, v := range []string{"<" + id + ">", id} {
		d, err := c.c.UIDSearch(&imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: v}},
		}, nil).Wait()
		if err != nil {
			return nil, fmt.Errorf("imap: search: %w", err)
		}
		hits := fromUIDs(d.AllUIDs())
		if len(hits) == 0 {
			continue
		}
		ids, err := c.envelopeIDs(hits)
		if err != nil {
			return nil, err
		}
		var out []uint32
		for _, u := range hits {
			if ids[u] == id {
				out = append(out, u)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, nil
}

// existing returns which of uids still exist in the selected mailbox, with one
// UID SEARCH over the whole set.
func (c *Conn) existing(uids []uint32) ([]uint32, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	d, err := c.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{uidSet(uids)}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: search uid: %w", err)
	}
	got := map[uint32]bool{}
	for _, u := range d.AllUIDs() {
		got[uint32(u)] = true
	}
	var out []uint32
	for _, u := range uids {
		if got[u] {
			out = append(out, u)
		}
	}
	return out, nil
}

// Locate finds a message by Message-ID: first checks uidHint's envelope, then searches.
// It records the mailbox's UIDVALIDITY; a later Erase on this Conn refuses if it changed.
func (c *Conn) Locate(ctx context.Context, mailbox, messageID string, uidHint uint32) (uint32, bool, error) {
	id := normID(messageID)
	if id == "" {
		return 0, false, errors.New("imap: empty Message-ID")
	}
	defer c.begin(ctx)()
	if err := c.selFresh(mailbox); err != nil {
		return 0, false, err
	}
	if uidHint != 0 {
		ids, err := c.envelopeIDs([]uint32{uidHint})
		if err != nil {
			return 0, false, err
		}
		if got, ok := ids[uidHint]; ok && got == id {
			return uidHint, true, nil
		}
	}
	hits, err := c.searchID(id)
	if err != nil || len(hits) == 0 {
		return 0, false, err
	}
	return hits[0], true, nil
}

// LocateItem is one message for LocateMany: its Message-ID and the UID Mail
// remembers for it (0 when unknown).
type LocateItem struct {
	MessageID string
	UIDHint   uint32
}

// LocateMany finds many messages in one mailbox: one SELECT, one ENVELOPE FETCH
// over all hints, and a Message-ID SEARCH only for items whose hint does not match.
// The result maps each found item's MessageID (exactly as given) to its UID;
// items not on the server are absent. Like Locate, it records UIDVALIDITY.
func (c *Conn) LocateMany(ctx context.Context, mailbox string, items []LocateItem) (map[string]uint32, error) {
	out := map[string]uint32{}
	if len(items) == 0 {
		return out, nil
	}
	for _, it := range items {
		if normID(it.MessageID) == "" {
			return nil, errors.New("imap: empty Message-ID")
		}
	}
	defer c.begin(ctx)()
	if err := c.selFresh(mailbox); err != nil {
		return nil, err
	}
	var hints []uint32
	for _, it := range items {
		if it.UIDHint != 0 && !slices.Contains(hints, it.UIDHint) {
			hints = append(hints, it.UIDHint)
		}
	}
	slices.Sort(hints)
	ids, err := c.envelopeIDs(hints)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		id := normID(it.MessageID)
		if it.UIDHint != 0 {
			if got, ok := ids[it.UIDHint]; ok && got == id {
				out[it.MessageID] = it.UIDHint
				continue
			}
		}
		hits, err := c.searchID(id)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			out[it.MessageID] = hits[0]
		}
	}
	return out, nil
}

// FetchRaw returns the full message with BODY.PEEK[] (does not set \Seen).
func (c *Conn) FetchRaw(ctx context.Context, mailbox string, uid uint32) ([]byte, error) {
	defer c.begin(ctx)()
	if err := c.sel(mailbox); err != nil {
		return nil, err
	}
	sec := &imap.FetchItemBodySection{Peek: true}
	msgs, err := c.c.Fetch(uidSet([]uint32{uid}), &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{sec},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: fetch body: %w", err)
	}
	for _, m := range msgs {
		if uint32(m.UID) == uid {
			if b := m.FindBodySection(sec); b != nil {
				return b, nil
			}
		}
	}
	return nil, fmt.Errorf("imap: message uid %d not found in %q", uid, mailbox)
}

// deleteAndExpunge sets \Deleted on uids and removes exactly them with UID EXPUNGE.
func (c *Conn) deleteAndExpunge(uids []uint32) error {
	set := uidSet(uids)
	if err := c.c.Store(set, &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		return fmt.Errorf("imap: store \\Deleted: %w", err)
	}
	if err := c.c.UIDExpunge(set).Close(); err != nil {
		return fmt.Errorf("imap: uid expunge: %w", err)
	}
	return nil
}

// clearDeleted removes \Deleted from uids (best effort; missing UIDs are ignored by
// the server), so messages that were not erased are not left marked for a later EXPUNGE.
func (c *Conn) clearDeleted(uids []uint32) {
	if len(uids) == 0 {
		return
	}
	_ = c.c.Store(uidSet(uids), &imap.StoreFlags{
		Op: imap.StoreFlagsDel, Silent: true, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close()
}

// expungeVerified erases uids from the selected mailbox and checks by UID that they
// are gone, retrying leftovers. gone holds only UIDs confirmed absent by a successful
// check; on an error the unconfirmed ones stay in left.
func (c *Conn) expungeVerified(uids []uint32) (gone, left []uint32, err error) {
	pending := slices.Clone(uids)
	for i := 0; i < eraseAttempts && len(pending) > 0; i++ {
		if err = c.deleteAndExpunge(pending); err == nil {
			var still []uint32
			if still, err = c.existing(pending); err == nil {
				pending = still
				continue
			}
		}
		c.clearDeleted(pending)
		return without(uids, pending), pending, err
	}
	c.clearDeleted(pending)
	return without(uids, pending), pending, nil
}

func without(all, drop []uint32) []uint32 {
	var out []uint32
	for _, u := range all {
		if !slices.Contains(drop, u) {
			out = append(out, u)
		}
	}
	return out
}

// Target is a message to erase: its UID and the Message-ID it must still carry.
type Target struct {
	UID       uint32
	MessageID string
}

// EraseResult says what Erase did with each target UID.
// Erased: confirmed gone. Missing: no such UID. Mismatched: the UID holds a message
// with another (or no) Message-ID, so it was not touched. Targets in none of these
// lists were attempted but not confirmed erased; the error then says why.
type EraseResult struct {
	Erased, Missing, Mismatched []uint32
}

// Erase permanently removes exactly the target messages from mailbox and checks
// by UID that they are gone. Before any change it fetches all target envelopes in
// one command and acts only on UIDs whose Message-ID matches. On an error the result
// still lists what was confirmed erased.
//
// Non-Gmail: \Deleted + UID EXPUNGE (UIDPLUS is required: a plain EXPUNGE could
// remove other \Deleted messages). Gmail: UID MOVE to Trash, then \Deleted +
// UID EXPUNGE of the moved copies there.
func (c *Conn) Erase(ctx context.Context, mailbox string, targets []Target, gmail bool) (EraseResult, error) {
	var res EraseResult
	if len(targets) == 0 {
		return res, nil
	}
	defer c.begin(ctx)()
	if !c.c.Caps().Has(imap.CapUIDPlus) {
		return res, errors.New("imap: server lacks UIDPLUS (UID EXPUNGE); refusing to erase")
	}
	trash := ""
	if gmail {
		t, err := c.specialUse(imap.MailboxAttrTrash)
		if err != nil {
			return res, err
		}
		if t == "" {
			t = gmailTrash
		}
		if t != mailbox {
			trash = t
		}
	}
	if err := c.selFresh(mailbox); err != nil {
		return res, err
	}
	var uids []uint32
	want := map[uint32]string{}
	for _, t := range targets {
		if _, dup := want[t.UID]; !dup {
			uids = append(uids, t.UID)
		}
		want[t.UID] = normID(t.MessageID)
	}
	slices.Sort(uids)
	ids, err := c.envelopeIDs(uids)
	if err != nil {
		return res, err
	}
	var ok []uint32
	for _, u := range uids {
		got, exists := ids[u]
		switch {
		case !exists:
			res.Missing = append(res.Missing, u)
		case got == "" || got != want[u]:
			res.Mismatched = append(res.Mismatched, u)
		default:
			ok = append(ok, u)
		}
	}
	if len(ok) == 0 {
		return res, nil
	}
	if trash == "" {
		gone, left, err := c.expungeVerified(ok)
		res.Erased = gone
		if err == nil && len(left) > 0 {
			err = fmt.Errorf("imap: %d message(s) still in %q after %d attempts: %v", len(left), mailbox, eraseAttempts, left)
		}
		return res, err
	}
	res.Erased, err = c.eraseViaTrash(mailbox, trash, ok, ids)
	return res, err
}

func (c *Conn) eraseViaTrash(mailbox, trash string, targets []uint32, ids map[uint32]string) ([]uint32, error) {
	st, err := c.c.Status(trash, &imap.StatusOptions{UIDNext: true}).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: status %q: %w", trash, err)
	}
	trashFloor := uint32(st.UIDNext)

	dest := map[uint32]uint32{} // source UID -> UID in Trash
	pending := slices.Clone(targets)
	var moveErr error
	for i := 0; i < eraseAttempts && len(pending) > 0; i++ {
		md, err := c.c.Move(uidSet(pending), trash).Wait()
		if err != nil {
			moveErr = fmt.Errorf("imap: move to %q: %w", trash, err)
		} else {
			if !c.noCopyUID {
				mapCopyUID(md, dest)
			}
			var still []uint32
			if still, err = c.existing(pending); err == nil {
				pending = still
				continue
			}
			moveErr = fmt.Errorf("imap: checking %q after move to %q (some messages may already be in %s, not erased): %w", mailbox, trash, trash, err)
		}
		break
	}
	moved := without(targets, pending)
	if moveErr == nil && len(pending) > 0 {
		moveErr = fmt.Errorf("imap: %d message(s) could not be moved out of %q: %v", len(pending), mailbox, pending)
	}
	if moveErr != nil && len(pending) == len(targets) {
		return nil, moveErr
	}

	// Trash UIDs to erase, each tied to its source UID.
	trashOf := map[uint32][]uint32{}
	if err := c.sel(trash); err != nil {
		return nil, joinErr(moveErr, fmt.Errorf("%w (%d message(s) moved to %s but not erased: %v)", err, len(moved), trash, moved))
	}
	for _, src := range moved {
		if d, ok := dest[src]; ok {
			trashOf[src] = []uint32{d}
			continue
		}
		// No COPYUID: find the moved copy by Message-ID among UIDs created by this move.
		hits, err := c.searchID(ids[src])
		if err != nil {
			return nil, joinErr(moveErr, fmt.Errorf("imap: locating moved copies in %s (%d message(s) moved there but not erased: %v): %w", trash, len(moved), moved, err))
		}
		for _, h := range hits {
			if h >= trashFloor {
				trashOf[src] = append(trashOf[src], h)
			}
		}
	}
	var trashUIDs []uint32
	for _, l := range trashOf {
		trashUIDs = append(trashUIDs, l...)
	}
	slices.Sort(trashUIDs)
	left, expErr := []uint32(nil), error(nil)
	if len(trashUIDs) > 0 {
		_, left, expErr = c.expungeVerified(trashUIDs)
	}

	var erased, stuck []uint32
	for _, src := range moved {
		tl, ok := trashOf[src]
		if !ok || slices.ContainsFunc(tl, func(u uint32) bool { return slices.Contains(left, u) }) {
			stuck = append(stuck, src)
			continue
		}
		erased = append(erased, src)
	}
	var trashErr error
	if len(stuck) > 0 {
		trashErr = fmt.Errorf("imap: %d message(s) moved to %s but not erased there (source UIDs %v)", len(stuck), trash, stuck)
		if expErr != nil {
			trashErr = fmt.Errorf("%w: %w", trashErr, expErr)
		}
	} else if expErr != nil {
		trashErr = expErr
	}
	return erased, joinErr(moveErr, trashErr)
}

func joinErr(a, b error) error {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return fmt.Errorf("%w; %w", a, b)
}

// mapCopyUID records source->destination UIDs from a COPYUID response.
func mapCopyUID(md *imapclient.MoveData, dest map[uint32]uint32) {
	if md == nil || md.SourceUIDs == nil || md.DestUIDs == nil {
		return
	}
	src, ok1 := md.SourceUIDs.(imap.UIDSet)
	dst, ok2 := md.DestUIDs.(imap.UIDSet)
	if !ok1 || !ok2 {
		return
	}
	s, ok1 := src.Nums()
	d, ok2 := dst.Nums()
	if !ok1 || !ok2 || len(s) != len(d) {
		return
	}
	for i := range s {
		dest[uint32(s[i])] = uint32(d[i])
	}
}

// Move moves uids from one mailbox to another. Without MOVE it needs UIDPLUS, so
// the COPY fallback removes only these messages.
func (c *Conn) Move(ctx context.Context, from, to string, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	defer c.begin(ctx)()
	caps := c.c.Caps()
	if !caps.Has(imap.CapMove) && !caps.Has(imap.CapUIDPlus) {
		return errors.New("imap: server lacks MOVE and UIDPLUS; refusing to move")
	}
	if err := c.sel(from); err != nil {
		return err
	}
	if _, err := c.c.Move(uidSet(uids), to).Wait(); err != nil {
		return fmt.Errorf("imap: move %q -> %q: %w", from, to, err)
	}
	return nil
}

// SetFlags adds (on) or removes \Seen or \Flagged on uids.
func (c *Conn) SetFlags(ctx context.Context, mailbox string, uids []uint32, flag string, on bool) error {
	f := imap.Flag(flag)
	if f != imap.FlagSeen && f != imap.FlagFlagged {
		return fmt.Errorf("imap: flag %q not allowed", flag)
	}
	if len(uids) == 0 {
		return nil
	}
	defer c.begin(ctx)()
	if err := c.sel(mailbox); err != nil {
		return err
	}
	op := imap.StoreFlagsDel
	if on {
		op = imap.StoreFlagsAdd
	}
	if err := c.c.Store(uidSet(uids), &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{f}}, nil).Close(); err != nil {
		return fmt.Errorf("imap: store %s: %w", flag, err)
	}
	return nil
}

// Append stores raw in mailbox, optionally with \Seen.
func (c *Conn) Append(ctx context.Context, mailbox string, raw []byte, seen bool) error {
	defer c.begin(ctx)()
	o := &imap.AppendOptions{}
	if seen {
		o.Flags = []imap.Flag{imap.FlagSeen}
	}
	cmd := c.c.Append(mailbox, int64(len(raw)), o)
	if _, err := bytes.NewReader(raw).WriteTo(cmd); err != nil {
		cmd.Close()
		return fmt.Errorf("imap: append: %w", err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("imap: append: %w", err)
	}
	if _, err := cmd.Wait(); err != nil {
		return fmt.Errorf("imap: append to %q: %w", mailbox, err)
	}
	return nil
}

// SpecialUse returns the mailbox carrying a SPECIAL-USE attribute (\Trash, \Sent,
// \Junk, \Archive, ...), or "" when the server marks none.
func (c *Conn) SpecialUse(ctx context.Context, use string) (string, error) {
	defer c.begin(ctx)()
	return c.specialUse(imap.MailboxAttr(use))
}

func (c *Conn) specialUse(use imap.MailboxAttr) (string, error) {
	var o *imap.ListOptions
	if c.c.Caps().Has(imap.CapListExtended) {
		o = &imap.ListOptions{ReturnSpecialUse: true}
	}
	l, err := c.c.List("", "*", o).Collect()
	if err != nil {
		return "", fmt.Errorf("imap: list: %w", err)
	}
	for _, d := range l {
		for _, a := range d.Attrs {
			if strings.EqualFold(string(a), string(use)) {
				return d.Mailbox, nil
			}
		}
	}
	return "", nil
}

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
	if _, err := c.c.Select(mailbox, nil).Wait(); err != nil {
		c.selected = ""
		return fmt.Errorf("imap: select %q: %w", mailbox, err)
	}
	c.selected = mailbox
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

// existing returns which of uids still exist in the selected mailbox.
func (c *Conn) existing(uids []uint32) ([]uint32, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	d, err := c.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{uidSet(uids)}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: search uid: %w", err)
	}
	got := fromUIDs(d.AllUIDs())
	var out []uint32
	for _, u := range uids {
		if slices.Contains(got, u) {
			out = append(out, u)
		}
	}
	return out, nil
}

// leftovers returns which of pending are still in the selected mailbox: by UID,
// and by a Message-ID search limited to pending UIDs (a duplicate with the same
// Message-ID that was not asked for is never counted, so never touched).
func (c *Conn) leftovers(pending []uint32, ids map[uint32]string) ([]uint32, error) {
	left, err := c.existing(pending)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, u := range pending {
		id := ids[u]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		hits, err := c.searchID(id)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			if slices.Contains(pending, h) && !slices.Contains(left, h) {
				left = append(left, h)
			}
		}
	}
	slices.Sort(left)
	return left, nil
}

// Locate finds a message by Message-ID: first checks uidHint's envelope, then searches.
func (c *Conn) Locate(ctx context.Context, mailbox, messageID string, uidHint uint32) (uint32, bool, error) {
	id := normID(messageID)
	if id == "" {
		return 0, false, errors.New("imap: empty Message-ID")
	}
	defer c.begin(ctx)()
	if err := c.sel(mailbox); err != nil {
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

// expungeVerified erases uids from the selected mailbox, retrying leftovers,
// and returns the UIDs that are confirmed gone and those that remain.
func (c *Conn) expungeVerified(uids []uint32, ids map[uint32]string) (gone, left []uint32, err error) {
	pending := slices.Clone(uids)
	for i := 0; i < eraseAttempts && len(pending) > 0; i++ {
		if err := c.deleteAndExpunge(pending); err != nil {
			return without(uids, pending), pending, err
		}
		if pending, err = c.leftovers(pending, ids); err != nil {
			return without(uids, pending), pending, err
		}
	}
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

func present(uids []uint32, ids map[uint32]string) []uint32 {
	var out []uint32
	for _, u := range uids {
		if _, ok := ids[u]; ok && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	slices.Sort(out)
	return out
}

// Erase permanently removes exactly uids from mailbox and verifies they are gone.
// UIDs that do not exist are skipped. It returns the UIDs actually erased; an error
// means some messages could not be erased (they are not in the result).
//
// Non-Gmail: \Deleted + UID EXPUNGE (UIDPLUS is required: a plain EXPUNGE could
// remove other \Deleted messages). Gmail: UID MOVE to Trash, then \Deleted +
// UID EXPUNGE of the moved copies there.
func (c *Conn) Erase(ctx context.Context, mailbox string, uids []uint32, gmail bool) ([]uint32, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	defer c.begin(ctx)()
	if !c.c.Caps().Has(imap.CapUIDPlus) {
		return nil, errors.New("imap: server lacks UIDPLUS (UID EXPUNGE); refusing to erase")
	}
	trash := ""
	if gmail {
		t, err := c.specialUse(imap.MailboxAttrTrash)
		if err != nil {
			return nil, err
		}
		if t == "" {
			t = gmailTrash
		}
		if t != mailbox {
			trash = t
		}
	}
	if err := c.sel(mailbox); err != nil {
		return nil, err
	}
	ids, err := c.envelopeIDs(uids)
	if err != nil {
		return nil, err
	}
	targets := present(uids, ids)
	if len(targets) == 0 {
		return nil, nil
	}
	if trash == "" {
		gone, left, err := c.expungeVerified(targets, ids)
		if err == nil && len(left) > 0 {
			err = fmt.Errorf("imap: %d message(s) still in %q after %d attempts: %v", len(left), mailbox, eraseAttempts, left)
		}
		return gone, err
	}
	return c.eraseViaTrash(mailbox, trash, targets, ids)
}

func (c *Conn) eraseViaTrash(mailbox, trash string, targets []uint32, ids map[uint32]string) ([]uint32, error) {
	st, err := c.c.Status(trash, &imap.StatusOptions{UIDNext: true}).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: status %q: %w", trash, err)
	}
	trashFloor := uint32(st.UIDNext)

	dest := map[uint32]uint32{} // source UID -> UID in Trash
	pending := slices.Clone(targets)
	for i := 0; i < eraseAttempts && len(pending) > 0; i++ {
		md, err := c.c.Move(uidSet(pending), trash).Wait()
		if err != nil {
			return nil, fmt.Errorf("imap: move to %q: %w", trash, err)
		}
		if !c.noCopyUID {
			mapCopyUID(md, dest)
		}
		if pending, err = c.leftovers(pending, ids); err != nil {
			return nil, err
		}
	}
	moved := without(targets, pending)

	// Trash UIDs to erase, each tied to its source UID.
	trashOf := map[uint32][]uint32{}
	if err := c.sel(trash); err != nil {
		return nil, err
	}
	for _, src := range moved {
		if d, ok := dest[src]; ok {
			trashOf[src] = []uint32{d}
			continue
		}
		// No COPYUID: find the moved copy by Message-ID among UIDs created by this move.
		hits, err := c.searchID(ids[src])
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			if h >= trashFloor {
				trashOf[src] = append(trashOf[src], h)
			}
		}
	}
	var trashUIDs []uint32
	trashIDs := map[uint32]string{}
	for src, l := range trashOf {
		for _, u := range l {
			trashUIDs = append(trashUIDs, u)
			trashIDs[u] = ids[src]
		}
	}
	slices.Sort(trashUIDs)
	var left []uint32
	if len(trashUIDs) > 0 {
		if _, left, err = c.expungeVerified(trashUIDs, trashIDs); err != nil {
			return nil, err
		}
	}

	var erased []uint32
	var stuck []uint32
	for _, src := range moved {
		tl, ok := trashOf[src]
		if !ok || slices.ContainsFunc(tl, func(u uint32) bool { return slices.Contains(left, u) }) {
			stuck = append(stuck, src)
			continue
		}
		erased = append(erased, src)
	}
	switch {
	case len(pending) > 0:
		return erased, fmt.Errorf("imap: %d message(s) could not be moved out of %q: %v", len(pending), mailbox, pending)
	case len(stuck) > 0:
		return erased, fmt.Errorf("imap: %d message(s) moved to %q but not erased there: %v", len(stuck), trash, stuck)
	}
	return erased, nil
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

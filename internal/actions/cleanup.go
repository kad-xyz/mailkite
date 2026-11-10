// Package actions builds previews (plans) of mail changes and executes them after
// the user confirms them in a macOS dialog: erase (archived first), move to Trash,
// flags and moves. Only the previewed set is ever touched.
package actions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/imapx"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/message"
	"github.com/kad-xyz/mailkite/internal/safety"
	"github.com/kad-xyz/mailkite/internal/secret"
)

// IMAP is what the engine needs from an IMAP connection (*imapx.Conn).
type IMAP interface {
	LocateMany(ctx context.Context, mailbox string, items []imapx.LocateItem) (map[string]uint32, error)
	FetchRaw(ctx context.Context, mailbox string, uid uint32) ([]byte, error)
	Erase(ctx context.Context, mailbox string, targets []imapx.Target, gmail bool) (imapx.EraseResult, error)
	Move(ctx context.Context, from, to string, uids []uint32) error
	SetFlags(ctx context.Context, mailbox string, uids []uint32, flag string, on bool) error
	SpecialUse(ctx context.Context, use string) (string, error)
	Close() error
}

var _ IMAP = (*imapx.Conn)(nil)

// DialIMAP is the default Engine.Dial: imapx.Dial.
func DialIMAP(ctx context.Context, a config.Account, password string) (IMAP, error) {
	c, err := imapx.Dial(ctx, a, password)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Engine builds and executes plans. Connections are opened per Execute (or per
// immediate mark) and closed before it returns.
type Engine struct {
	Store   *maildata.Store
	Cfg     *config.Config
	Secrets secret.Store
	Plans   *safety.Plans
	Confirm safety.Confirmer
	Archive *safety.Archive
	State   *safety.State
	Dial    func(ctx context.Context, a config.Account, password string) (IMAP, error) // nil: DialIMAP
	POP     POPDeleter
	Now     func() time.Time // nil: time.Now
}

// CleanupRequest selects messages for a bulk action.
type CleanupRequest struct {
	Filter      maildata.Filter
	Senders     []string // addresses or @domains (a domain also matches its subdomains)
	Action      string   // erase|trash|read|move
	MoveTo      string
	Unsubscribe bool
}

// SenderCount is one line of a preview: how many messages a sender has in the plan.
type SenderCount struct {
	Sender string
	Count  int
}

// ProtectedMsg is a message a bulk action leaves alone, with Classify's reason.
type ProtectedMsg struct {
	ID            int64
	From, Subject string
	Reason        string
}

// Preview describes a plan. PlanID is empty when there is nothing to do or when a
// mark was executed at once (then Result holds its report).
type Preview struct {
	PlanID    string
	Count     int
	BySender  []SenderCount
	Protected []ProtectedMsg
	Skipped   []string
	Button    string
	Result    *Report
}

// Report is the outcome of Execute. PerAccount holds one line per account (by
// account name) with its counts and, on failure, why.
type Report struct {
	Done, NotFound, Failed int
	PerAccount             []string
	Cancelled              bool
}

const (
	cleanupPrefix = "cleanup:"
	markPrefix    = "mark:"
	pageSize      = 200
	dialogSenders = 10
	notSetUp      = "account not set up (run mailkite setup)"
	popOnlyErase  = "POP account: only erase is supported"
)

// maxCleanup caps one cleanup preview (a variable for tests).
var maxCleanup = 5000

// planExtra is the Plan.Extra of cleanup and mark plans.
type planExtra struct {
	Op          string // erase|trash|read|unread|flag|unflag|move
	MoveTo      string
	Unsubscribe bool
	Names       map[string]string // account UUID -> account name, for reports
	Protected   int
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) markLimit() int {
	if e.Cfg.MarkLimit > 0 {
		return e.Cfg.MarkLimit
	}
	return config.DefaultMarkLimit
}

func (e *Engine) hidden(ctx context.Context) (map[string]struct{}, error) {
	if e.State == nil {
		return map[string]struct{}{}, nil
	}
	return e.State.Hidden(ctx)
}

type candidate struct {
	sm maildata.Summary
	mb maildata.Mailbox
}

// senderMatcher turns "a@b.c" into an exact address match and "@b.c" or "b.c" into
// a domain match that includes subdomains. needle is the Filter.From substring
// that finds a superset of the matches in the index.
func senderMatcher(spec string) (needle string, match func(addr string) bool) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" {
		return "", nil
	}
	if !strings.HasPrefix(spec, "@") && strings.Contains(spec, "@") {
		return spec, func(addr string) bool { return strings.EqualFold(strings.TrimSpace(addr), spec) }
	}
	d := strings.TrimPrefix(spec, "@")
	return d, func(addr string) bool {
		_, dom, ok := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
		return ok && (dom == d || strings.HasSuffix(dom, "."+d))
	}
}

func hasConstraint(r CleanupRequest) bool {
	f := r.Filter
	for _, s := range r.Senders {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return f.From != "" || f.Subject != "" || f.Mailbox != "" || f.Role != "" || !f.Since.IsZero() ||
		!f.Until.IsZero() || f.Unread != nil || f.Flagged != nil || f.HasAttachments != nil || len(f.IDs) > 0
}

var errTooMany = func() error {
	return fmt.Errorf("more than %d messages match; narrow the filter (sender, dates, mailbox)", maxCleanup)
}

// collect pages through every match of r (deduplicated), at most maxCleanup.
func (e *Engine) collect(ctx context.Context, r CleanupRequest) ([]candidate, error) {
	type spec struct {
		needle string
		match  func(string) bool
	}
	var specs []spec
	for _, s := range r.Senders {
		if n, m := senderMatcher(s); m != nil {
			specs = append(specs, spec{n, m})
		}
	}
	if len(specs) == 0 {
		specs = []spec{{}}
	}
	fromNeedle := strings.ToLower(r.Filter.From)
	seen := map[string]bool{}
	var out []candidate
	for _, sp := range specs {
		f := r.Filter
		f.Limit = pageSize
		if sp.match != nil {
			f.From = sp.needle
		}
		scanned := 0
		for off := 0; ; off += pageSize {
			f.Offset = off
			items, more, err := e.Store.Find(ctx, f)
			if err != nil {
				return nil, err
			}
			scanned += len(items)
			if scanned > maxCleanup {
				return nil, errTooMany()
			}
			for _, sm := range items {
				if sp.match != nil && !sp.match(sm.From) {
					continue
				}
				if sp.match != nil && fromNeedle != "" && !strings.Contains(strings.ToLower(sm.From), fromNeedle) &&
					!strings.Contains(strings.ToLower(sm.FromName), fromNeedle) {
					continue
				}
				k := fmt.Sprintf("r:%d", sm.ID)
				if sm.MessageID != "" {
					k = "m:" + sm.MessageID
				}
				if seen[k] {
					continue
				}
				seen[k] = true
				_, mb, err := e.Store.Get(ctx, sm.ID)
				if errors.Is(err, maildata.ErrNotFound) {
					continue // gone from Mail's index meanwhile
				}
				if err != nil {
					return nil, err
				}
				out = append(out, candidate{sm, mb})
				if len(out) > maxCleanup {
					return nil, errTooMany()
				}
			}
			if !more {
				break
			}
		}
	}
	return out, nil
}

// parseLocal parses the local .emlx (partial ones too); nil when unreadable, which
// Classify treats as protected.
func (e *Engine) parseLocal(id int64, mb maildata.Mailbox) *message.Message {
	p, _, ok := e.Store.EmlxPath(id, mb)
	if !ok {
		return nil
	}
	raw, err := message.ReadEmlx(p)
	if err != nil {
		return nil
	}
	m, err := message.Parse(raw)
	if err != nil {
		return nil
	}
	return m
}

// builder accumulates plan targets account by account.
type builder struct {
	targets map[string][]safety.Target
	names   map[string]string
	senders map[string]int
	skipped map[string]map[string]int // account name -> reason -> count
	count   int
	byAcc   map[string]int
}

func newBuilder() *builder {
	return &builder{targets: map[string][]safety.Target{}, names: map[string]string{}, senders: map[string]int{},
		skipped: map[string]map[string]int{}, byAcc: map[string]int{}}
}

func (b *builder) skip(name, reason string) {
	if b.skipped[name] == nil {
		b.skipped[name] = map[string]int{}
	}
	b.skipped[name][reason]++
}

// add checks that the message can be acted on for op and records it as a target.
func (e *Engine) add(ctx context.Context, b *builder, sm maildata.Summary, mb maildata.Mailbox, op string) error {
	uuid := mb.AccountUUID
	name := sm.Account
	acc, ok := e.Cfg.ByMailUUID(uuid)
	switch {
	case !ok:
		b.skip(name, notSetUp)
		return nil
	case acc.Kind == "pop" && op != "erase":
		b.skip(name, popOnlyErase)
		return nil
	case strings.TrimSpace(sm.MessageID) == "":
		b.skip(name, "no Message-ID, cannot be matched on the server")
		return nil
	}
	uid, err := e.Store.RemoteID(ctx, sm.ID)
	if err != nil && !errors.Is(err, maildata.ErrNotFound) {
		return err
	}
	b.targets[uuid] = append(b.targets[uuid], safety.Target{ID: sm.ID, MessageID: sm.MessageID, Mailbox: mb.Name,
		UID: uid, From: sm.From, Subject: sm.Subject})
	b.names[uuid] = name
	b.senders[strings.ToLower(sm.From)]++
	b.byAcc[name]++
	b.count++
	return nil
}

func (b *builder) preview(p *Preview) {
	p.Count = b.count
	for s, n := range b.senders {
		p.BySender = append(p.BySender, SenderCount{s, n})
	}
	sort.Slice(p.BySender, func(i, j int) bool {
		if p.BySender[i].Count != p.BySender[j].Count {
			return p.BySender[i].Count > p.BySender[j].Count
		}
		return p.BySender[i].Sender < p.BySender[j].Sender
	})
	for name, reasons := range b.skipped {
		for r, n := range reasons {
			p.Skipped = append(p.Skipped, fmt.Sprintf("%s: %d %s skipped: %s", name, n, plural(n, "message", "messages", "messages"), r))
		}
	}
	sort.Strings(p.Skipped)
}

func validOp(op, moveTo string, ops ...string) error {
	found := false
	for _, o := range ops {
		found = found || o == op
	}
	if !found {
		return fmt.Errorf("unknown action %q (want %s)", op, strings.Join(ops, "|"))
	}
	if op == "move" && strings.TrimSpace(moveTo) == "" {
		return errors.New("action move needs a target mailbox (move_to)")
	}
	return nil
}

// BuildCleanup previews a bulk action: every match of the filter and senders
// (at most 5000), minus protected and already-erased (hidden) messages. It changes
// nothing and stores a plan for Execute.
func (e *Engine) BuildCleanup(ctx context.Context, r CleanupRequest) (Preview, error) {
	if err := validOp(r.Action, r.MoveTo, "erase", "trash", "read", "move"); err != nil {
		return Preview{}, err
	}
	if !hasConstraint(r) {
		return Preview{}, errors.New("cleanup needs senders or a filter (from, subject, mailbox, dates, unread...); it never acts on all mail")
	}
	cands, err := e.collect(ctx, r)
	if err != nil {
		return Preview{}, err
	}
	hidden, err := e.hidden(ctx)
	if err != nil {
		return Preview{}, err
	}
	var p Preview
	b := newBuilder()
	for _, c := range cands {
		if _, h := hidden[c.sm.MessageID]; h && c.sm.MessageID != "" {
			continue
		}
		if prot, why := safety.Classify(e.parseLocal(c.sm.ID, c.mb), c.sm, e.Cfg.Rules); prot {
			p.Protected = append(p.Protected, ProtectedMsg{ID: c.sm.ID, From: c.sm.From, Subject: c.sm.Subject, Reason: why})
			continue
		}
		if err := e.add(ctx, b, c.sm, c.mb, r.Action); err != nil {
			return Preview{}, err
		}
	}
	b.preview(&p)
	if b.count == 0 {
		return p, nil
	}
	x := planExtra{Op: r.Action, MoveTo: r.MoveTo, Unsubscribe: r.Unsubscribe, Names: b.names, Protected: len(p.Protected)}
	if err := e.store(&p, cleanupPrefix+r.Action, b, x); err != nil {
		return Preview{}, err
	}
	return p, nil
}

func (e *Engine) newPlan(action string, b *builder, x planExtra, p *Preview) (safety.Plan, error) {
	p.Button = button(x.Op, b.count)
	return safety.NewPlanFor(action, b.targets, summary(x, p, b.byAcc), p.Button, x, e.now())
}

func (e *Engine) store(p *Preview, action string, b *builder, x planExtra) error {
	plan, err := e.newPlan(action, b, x, p)
	if err != nil {
		return err
	}
	p.PlanID = e.Plans.New(plan)
	return nil
}

// BuildMark previews flag changes or a move for explicitly listed messages
// (protected ones included: they were picked one by one). Up to Cfg.MarkLimit
// messages are changed at once without a dialog, except a move to Trash or Junk;
// otherwise a plan is stored for Execute.
func (e *Engine) BuildMark(ctx context.Context, ids []int64, action, moveTo string) (Preview, bool, error) {
	if err := validOp(action, moveTo, "read", "unread", "flag", "unflag", "move"); err != nil {
		return Preview{}, false, err
	}
	if len(ids) == 0 {
		return Preview{}, false, errors.New("mark needs message ids")
	}
	hidden, err := e.hidden(ctx)
	if err != nil {
		return Preview{}, false, err
	}
	var p Preview
	b := newBuilder()
	done := map[int64]bool{}
	for _, id := range ids {
		if done[id] {
			continue
		}
		done[id] = true
		sm, mb, err := e.Store.Get(ctx, id)
		if errors.Is(err, maildata.ErrNotFound) {
			p.Skipped = append(p.Skipped, fmt.Sprintf("id %d: not found", id))
			continue
		}
		if err != nil {
			return Preview{}, false, err
		}
		if _, h := hidden[sm.MessageID]; h && sm.MessageID != "" {
			p.Skipped = append(p.Skipped, fmt.Sprintf("id %d: already erased", id))
			continue
		}
		if acc, ok := e.Cfg.ByMailUUID(mb.AccountUUID); ok && acc.Kind == "pop" {
			b.skip(sm.Account, "POP account: marks are not supported")
			continue
		}
		if err := e.add(ctx, b, sm, mb, action); err != nil {
			return Preview{}, false, err
		}
	}
	extra := p.Skipped
	p.Skipped = nil
	b.preview(&p)
	p.Skipped = append(extra, p.Skipped...)
	if b.count == 0 {
		return p, false, nil
	}
	x := planExtra{Op: action, MoveTo: moveTo, Names: b.names}
	risky := action == "move" && (maildata.NameRole(moveTo) == maildata.RoleTrash || maildata.NameRole(moveTo) == maildata.RoleJunk)
	if b.count <= e.markLimit() && !risky {
		plan, err := e.newPlan(markPrefix+action, b, x, &p)
		if err != nil {
			return Preview{}, false, err
		}
		rep := e.run(ctx, plan, x)
		p.Result = &rep
		return p, true, nil
	}
	if err := e.store(&p, markPrefix+action, b, x); err != nil {
		return Preview{}, false, err
	}
	return p, false, nil
}

// Execute runs a plan once: it shows the macOS dialog and acts only when the user
// pressed the action button. Cancel, timeout or a dialog failure change nothing.
func (e *Engine) Execute(ctx context.Context, planID string) (Report, error) {
	plan, err := e.Plans.Take(planID, e.now())
	if err != nil {
		return Report{}, err
	}
	if err := plan.Verify(plan.Action, plan.Account); err != nil {
		return Report{}, err
	}
	x, ok := plan.Extra.(planExtra)
	if !ok || !(strings.HasPrefix(plan.Action, cleanupPrefix) || strings.HasPrefix(plan.Action, markPrefix)) {
		return Report{}, fmt.Errorf("plan action %q is not supported", plan.Action)
	}
	if x.Op == "erase" && (e.Archive == nil || e.Archive.Dir == "") {
		return Report{}, errors.New("no archive configured; refusing to erase")
	}
	if e.Confirm == nil {
		return Report{}, errors.New("no confirmation dialog available; nothing was changed")
	}
	yes, err := e.Confirm.Confirm(ctx, "mailkite", plan.Summary, plan.Button)
	if err != nil {
		e.logAll(plan, "dialog_failed")
		return Report{}, fmt.Errorf("confirmation dialog failed, nothing was changed: %w", err)
	}
	if !yes {
		e.logAll(plan, "cancelled")
		return Report{Cancelled: true}, nil
	}
	return e.run(ctx, plan, x), nil
}

func (e *Engine) logAll(plan safety.Plan, result string) {
	if e.State == nil {
		return
	}
	for _, uuid := range sortedKeys(plan.Account) {
		_ = e.State.Log(safety.ActionLine{TS: e.now(), Action: plan.Action, Account: uuid, Result: result})
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// result is one account's outcome.
type result struct {
	done, notFound, failed int
	notes                  []string
	code                   string // short status for actions.log (no addresses)
}

func (r *result) fail(n int, code, note string) {
	r.failed += n
	if r.code == "" {
		r.code = code
	}
	r.notes = append(r.notes, note)
}

// run acts on every account of the plan; one account's failure never stops the others.
func (e *Engine) run(ctx context.Context, plan safety.Plan, x planExtra) Report {
	var rep Report
	for _, uuid := range sortedKeys(plan.Account) {
		ts := plan.Account[uuid]
		name := x.Names[uuid]
		if name == "" {
			name = uuid
		}
		r := e.runAccount(ctx, uuid, ts, x)
		rep.Done += r.done
		rep.NotFound += r.notFound
		rep.Failed += r.failed
		line := fmt.Sprintf("%s: done %d, not found %d, failed %d", name, r.done, r.notFound, r.failed)
		if len(r.notes) > 0 {
			line += " (" + strings.Join(r.notes, "; ") + ")"
		}
		code := r.code
		if code == "" {
			code = "ok"
		}
		if e.State != nil {
			if err := e.State.Log(safety.ActionLine{TS: e.now(), Action: plan.Action, Account: uuid, Count: r.done,
				Result: fmt.Sprintf("%s done=%d not_found=%d failed=%d", code, r.done, r.notFound, r.failed)}); err != nil {
				line += " (actions.log not written)"
			}
		}
		rep.PerAccount = append(rep.PerAccount, line)
	}
	if x.Op == "erase" && e.Archive != nil {
		if err := e.Archive.Prune(e.now()); err != nil {
			rep.PerAccount = append(rep.PerAccount, "archive: old copies not pruned: "+err.Error())
		}
	}
	return rep
}

func (e *Engine) runAccount(ctx context.Context, uuid string, ts []safety.Target, x planExtra) result {
	var r result
	acc, ok := e.Cfg.ByMailUUID(uuid)
	if !ok {
		r.fail(len(ts), "not_set_up", notSetUp)
		return r
	}
	if acc.Kind == "pop" {
		return e.runPOP(ctx, uuid, ts, x)
	}
	if e.Secrets == nil {
		r.fail(len(ts), "no_password", "no password store")
		return r
	}
	pw, err := e.Secrets.Get(acc.Email)
	if err != nil {
		r.fail(len(ts), "no_password", "no password in Keychain (run mailkite setup)")
		return r
	}
	dial := e.Dial
	if dial == nil {
		dial = DialIMAP
	}
	conn, err := dial(ctx, acc, pw)
	if err != nil {
		r.fail(len(ts), "connect_failed", "cannot connect: "+err.Error())
		return r
	}
	defer conn.Close()

	byBox := map[string][]safety.Target{}
	for _, t := range ts {
		byBox[t.Mailbox] = append(byBox[t.Mailbox], t)
	}
	trash := ""
	for _, box := range sortedKeys(byBox) {
		bts := byBox[box]
		items := make([]imapx.LocateItem, len(bts))
		for i, t := range bts {
			items[i] = imapx.LocateItem{MessageID: t.MessageID, UIDHint: t.UID}
		}
		found, err := conn.LocateMany(ctx, box, items)
		if err != nil {
			r.fail(len(bts), "locate_failed", box+": "+err.Error())
			continue
		}
		var locs []located
		for _, t := range bts {
			if uid, ok := found[t.MessageID]; ok {
				locs = append(locs, located{t, uid})
			} else {
				r.notFound++
			}
		}
		if len(locs) == 0 {
			continue
		}
		switch x.Op {
		case "erase":
			e.eraseBox(ctx, conn, acc, uuid, box, locs, &r)
		case "trash":
			if trash == "" {
				if trash, err = e.trashBox(ctx, conn, acc, uuid); err != nil {
					r.fail(len(locs), "no_trash", err.Error())
					trash = ""
					continue
				}
			}
			e.moveBox(ctx, conn, box, trash, locs, &r)
		case "move":
			e.moveBox(ctx, conn, box, x.MoveTo, locs, &r)
		case "read", "unread", "flag", "unflag":
			flag, on := `\Seen`, x.Op == "read"
			if x.Op == "flag" || x.Op == "unflag" {
				flag, on = `\Flagged`, x.Op == "flag"
			}
			if err := conn.SetFlags(ctx, box, uidsOf(locs), flag, on); err != nil {
				r.fail(len(locs), "flags_failed", box+": "+err.Error())
				continue
			}
			r.done += len(locs)
		default:
			r.fail(len(locs), "unknown_action", "unknown action "+x.Op)
		}
	}
	return r
}

// located is a plan target with its UID confirmed on the server.
type located struct {
	t   safety.Target
	uid uint32
}

func uidsOf(ls []located) []uint32 {
	out := make([]uint32, len(ls))
	for i, l := range ls {
		out[i] = l.uid
	}
	return out
}

func (e *Engine) moveBox(ctx context.Context, conn IMAP, from, to string, ls []located, r *result) {
	if from == to {
		r.done += len(ls) // already there
		return
	}
	if err := conn.Move(ctx, from, to, uidsOf(ls)); err != nil {
		r.fail(len(ls), "move_failed", from+": "+err.Error())
		return
	}
	r.done += len(ls)
}

// trashBox finds the account's Trash: SPECIAL-USE \Trash, then Gmail's default,
// then a Mail mailbox of the account whose name means Trash.
func (e *Engine) trashBox(ctx context.Context, conn IMAP, acc config.Account, uuid string) (string, error) {
	t, err := conn.SpecialUse(ctx, `\Trash`)
	if err != nil {
		return "", err
	}
	if t != "" {
		return t, nil
	}
	if acc.Gmail {
		return "[Gmail]/Trash", nil
	}
	accs, err := e.Store.Accounts(ctx)
	if err != nil {
		return "", err
	}
	for _, a := range accs {
		if a.UUID != uuid {
			continue
		}
		for _, mb := range a.Mailboxes {
			if mb.Role == maildata.RoleTrash {
				return mb.Name, nil
			}
		}
	}
	return "", errors.New("no Trash mailbox found")
}

// archiveSummary is what the archive needs: the account UUID (not its name) goes
// into the sidecar.
func (e *Engine) archiveSummary(ctx context.Context, uuid string, t safety.Target) (maildata.Summary, maildata.Mailbox, bool) {
	sm := maildata.Summary{ID: t.ID, MessageID: t.MessageID, Subject: t.Subject, Account: uuid, Mailbox: t.Mailbox}
	got, mb, err := e.Store.Get(ctx, t.ID)
	if err != nil || got.MessageID != t.MessageID {
		return sm, maildata.Mailbox{}, false
	}
	sm.Date = got.Date
	return sm, mb, true
}

// localRaw returns the full local message when Mail has it and it is the target.
func (e *Engine) localRaw(id int64, mb maildata.Mailbox, messageID string) []byte {
	p, partial, ok := e.Store.EmlxPath(id, mb)
	if !ok || partial {
		return nil
	}
	raw, err := message.ReadEmlx(p)
	if err != nil || len(raw) == 0 {
		return nil
	}
	if m, err := message.Parse(raw); err != nil || normID(m.MessageID) != normID(messageID) {
		return nil
	}
	return raw
}

// eraseBox archives every located message of one mailbox and only then erases the
// archived ones. A message that cannot be archived is not erased.
func (e *Engine) eraseBox(ctx context.Context, conn IMAP, acc config.Account, uuid, box string, ls []located, r *result) {
	now := e.now()
	var targets []imapx.Target
	idOf := map[uint32]string{}
	unread := 0
	for _, l := range ls {
		sm, mb, inIndex := e.archiveSummary(ctx, uuid, l.t)
		var raw []byte
		if inIndex {
			raw = e.localRaw(l.t.ID, mb, l.t.MessageID)
		}
		if raw == nil {
			b, err := conn.FetchRaw(ctx, box, l.uid)
			if err != nil || len(b) == 0 {
				unread++
				continue
			}
			raw = b
		}
		if _, err := e.Archive.Save(now, sm, box, raw); err != nil {
			unread++
			continue
		}
		targets = append(targets, imapx.Target{UID: l.uid, MessageID: l.t.MessageID})
		idOf[l.uid] = l.t.MessageID
	}
	if unread > 0 {
		r.fail(unread, "archive_failed", fmt.Sprintf("%s: %d not archived, so not erased", box, unread))
	}
	if len(targets) == 0 {
		return
	}
	res, err := conn.Erase(ctx, box, targets, acc.Gmail)
	r.done += len(res.Erased)
	r.notFound += len(res.Missing)
	if n := len(res.Mismatched); n > 0 {
		r.fail(n, "changed", fmt.Sprintf("%s: %d changed on server", box, n))
	}
	if rest := len(targets) - len(res.Erased) - len(res.Missing) - len(res.Mismatched); rest > 0 {
		note := fmt.Sprintf("%s: %d not confirmed erased", box, rest)
		if err != nil {
			note += ": " + err.Error()
		}
		r.fail(rest, "erase_failed", note)
	} else if err != nil {
		r.notes = append(r.notes, box+": "+err.Error())
	}
	var ids []string
	for _, u := range res.Erased {
		ids = append(ids, idOf[u])
	}
	e.hide(ids, now, r)
}

func (e *Engine) hide(ids []string, now time.Time, r *result) {
	if len(ids) == 0 || e.State == nil {
		return
	}
	if err := e.State.Hide(ids, now); err != nil {
		r.notes = append(r.notes, "erased messages may still show until Mail syncs: "+err.Error())
	}
}

// runPOP archives POP messages from Mail's local copies, then deletes them in Mail.
func (e *Engine) runPOP(ctx context.Context, uuid string, ts []safety.Target, x planExtra) result {
	var r result
	if x.Op != "erase" {
		r.fail(len(ts), "unsupported", popOnlyErase)
		return r
	}
	if e.POP == nil {
		r.fail(len(ts), "unsupported", "POP deletion is not available")
		return r
	}
	now := e.now()
	var ids []string
	unread := 0
	for _, t := range ts {
		sm, mb, inIndex := e.archiveSummary(ctx, uuid, t)
		if !inIndex {
			r.notFound++ // no longer in Mail
			continue
		}
		raw := e.localRaw(t.ID, mb, t.MessageID)
		if raw == nil {
			unread++
			continue
		}
		if _, err := e.Archive.Save(now, sm, t.Mailbox, raw); err != nil {
			unread++
			continue
		}
		ids = append(ids, t.MessageID)
	}
	if unread > 0 {
		r.fail(unread, "archive_failed", fmt.Sprintf("%d not archived, so not deleted", unread))
	}
	if len(ids) == 0 {
		return r
	}
	n, err := e.POP.Delete(ctx, uuid, ids)
	n = max(0, min(n, len(ids)))
	r.done += n
	if err != nil {
		r.fail(len(ids)-n, "pop_failed", "Mail: "+err.Error())
		return r
	}
	r.notFound += len(ids) - n
	if n == len(ids) {
		e.hide(ids, now, &r)
	}
	return r
}

// plural picks the Russian (or English: one, many, many) form for n.
func plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	}
	return many
}

func letters(n int) string {
	return fmt.Sprintf("%d %s", n, plural(n, "письмо", "письма", "писем"))
}

func button(op string, n int) string {
	switch op {
	case "erase":
		return fmt.Sprintf("Стереть %d", n)
	case "trash":
		return fmt.Sprintf("В корзину: %d", n)
	case "read":
		return fmt.Sprintf("Прочитано: %d", n)
	case "unread":
		return fmt.Sprintf("Непрочитано: %d", n)
	case "flag":
		return fmt.Sprintf("Флаг: %d", n)
	case "unflag":
		return fmt.Sprintf("Снять флаг: %d", n)
	}
	return fmt.Sprintf("Переместить %d", n)
}

// summary is the dialog body: what happens, to how many messages, from whom.
func summary(x planExtra, p *Preview, byAcc map[string]int) string {
	var b strings.Builder
	n := p.Count
	switch x.Op {
	case "erase":
		fmt.Fprintf(&b, "Стереть насовсем %s. Копии .eml останутся в архиве mailkite на 30 дней.", letters(n))
	case "trash":
		fmt.Fprintf(&b, "Переместить в Корзину %s.", letters(n))
	case "read":
		fmt.Fprintf(&b, "Пометить прочитанными %s.", letters(n))
	case "unread":
		fmt.Fprintf(&b, "Пометить непрочитанными %s.", letters(n))
	case "flag":
		fmt.Fprintf(&b, "Поставить флаг: %s.", letters(n))
	case "unflag":
		fmt.Fprintf(&b, "Снять флаг: %s.", letters(n))
	case "move":
		fmt.Fprintf(&b, "Переместить %s в «%s».", letters(n), x.MoveTo)
	}
	b.WriteString("\n\nОтправители:")
	for i, s := range p.BySender {
		if i == dialogSenders {
			rest, restN := len(p.BySender)-i, 0
			for _, r := range p.BySender[i:] {
				restN += r.Count
			}
			fmt.Fprintf(&b, "\n…и ещё %d (%s)", rest, letters(restN))
			break
		}
		fmt.Fprintf(&b, "\n%d — %s", s.Count, s.Sender)
	}
	var accs []string
	for _, name := range sortedKeys(byAcc) {
		accs = append(accs, fmt.Sprintf("%s — %d", name, byAcc[name]))
	}
	b.WriteString("\n\nАккаунты: " + strings.Join(accs, ", "))
	if x.Protected > 0 {
		fmt.Fprintf(&b, "\nЗащищённые письма не тронуты: %d", x.Protected)
	}
	return b.String()
}

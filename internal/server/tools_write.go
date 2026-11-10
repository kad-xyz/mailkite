package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kad-xyz/mailkite/internal/actions"
	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/safety"
	"github.com/kad-xyz/mailkite/internal/secret"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	notSetUp      = "account not set up (run mailkite setup)"
	maxSenderRows = 50
	maxProtRows   = 20
)

var (
	falseVal  = false
	trueVal   = true
	markHints = &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &falseVal}
	execHints = &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &trueVal}
)

// erasePlans remembers which plans erase mail, so execute can point at the archive.
type erasePlans struct {
	mu sync.Mutex
	m  map[string]bool
}

func (e *erasePlans) add(id string) {
	if id == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m == nil || len(e.m) > 1000 {
		e.m = map[string]bool{}
	}
	e.m[id] = true
}

func (e *erasePlans) take(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	ok := e.m[id]
	delete(e.m, id)
	return ok
}

// stateDir is Options.StateDir or ~/Library/Application Support/mailkite. Without
// a home directory it is an error: nothing is ever written relative to the cwd.
func (h *handlers) stateDir() (string, error) {
	if h.opts.StateDir != "" {
		return h.opts.StateDir, nil
	}
	d, err := safety.DefaultRoot()
	if err != nil {
		return "", fmt.Errorf("no state directory: %w", err)
	}
	return d, nil
}

// hiddenFilter returns a Filter.Skip that hides the rows (ROWIDs) mailkite erased
// but Mail's index still lists. With unhide, rows Mail no longer has are dropped
// from the state first (only those ROWIDs are looked up). nil when nothing is hidden.
func (h *handlers) hiddenFilter(ctx context.Context, st *maildata.Store, unhide bool) func(maildata.Summary) bool {
	state, err := h.state.get()
	if err != nil {
		return nil
	}
	hidden, err := state.Hidden(ctx)
	if err != nil || len(hidden) == 0 {
		return nil
	}
	if unhide {
		rows := make([]int64, 0, len(hidden))
		for k := range hidden {
			rows = append(rows, k.RowID)
		}
		if present, err := st.PresentRows(ctx, rows); err == nil {
			gone := map[string][]int64{}
			for k := range hidden {
				if !present[k.RowID] {
					gone[k.Account] = append(gone[k.Account], k.RowID)
					delete(hidden, k)
				}
			}
			for acc, ids := range gone {
				_ = state.Unhide(acc, ids)
			}
		}
		if len(hidden) == 0 {
			return nil
		}
	}
	return func(sm maildata.Summary) bool {
		_, ok := hidden[safety.HiddenKey{Account: sm.AccountUUID, RowID: sm.ID}]
		return ok
	}
}

// engine builds the write engine for one call: the config is re-read, so accounts
// added by `mailkite setup` work without a restart; plans, state and archive are
// shared by all calls.
func (h *handlers) engine() (*actions.Engine, error) {
	st, err := h.store.get()
	if err != nil {
		return nil, err
	}
	state, err := h.state.get()
	if err != nil {
		return nil, err
	}
	if h.opts.Engine != nil {
		return h.opts.Engine(st, state)
	}
	dir, err := h.stateDir()
	if err != nil {
		return nil, err
	}
	path, err := config.Path()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return &actions.Engine{
		Store: st, Cfg: cfg, Secrets: secret.Keychain{}, Plans: h.plans, Confirm: safety.OSAScript{},
		Archive: &safety.Archive{Dir: filepath.Join(dir, "archive")}, State: state,
		POP: actions.AppleScriptPOP{}, Version: h.opts.Version,
	}, nil
}

// withEngine runs fn with the engine, or answers with the setup hint (not an error).
func (h *handlers) withEngine(fn func(*actions.Engine) (*mcp.CallToolResult, any, error)) (*mcp.CallToolResult, any, error) {
	eng, err := h.engine()
	if err != nil {
		return errResult(err)
	}
	if len(eng.Cfg.Accounts) == 0 {
		return textResult(notSetUp)
	}
	return fn(eng)
}

type cleanupInput struct {
	Senders          []string `json:"senders,omitempty" jsonschema:"addresses or @domains"`
	IDs              []int64  `json:"ids,omitempty" jsonschema:"message ids from find"`
	IncludeProtected []int64  `json:"include_protected,omitempty" jsonschema:"protected ids to include"`
	From             string   `json:"from,omitempty" jsonschema:"sender contains"`
	Subject          string   `json:"subject,omitempty" jsonschema:"subject contains"`
	Account          string   `json:"account,omitempty" jsonschema:"account name"`
	Mailbox          string   `json:"mailbox,omitempty" jsonschema:"mailbox name or role"`
	Since            string   `json:"since,omitempty" jsonschema:"YYYY-MM-DD"`
	Until            string   `json:"until,omitempty" jsonschema:"YYYY-MM-DD, inclusive"`
	Unread           bool     `json:"unread,omitempty" jsonschema:"only unread"`
	Action           string   `json:"action" jsonschema:"erase|trash|read|move"`
	MoveTo           string   `json:"move_to,omitempty" jsonschema:"target mailbox for move"`
	Unsubscribe      bool     `json:"unsubscribe,omitempty" jsonschema:"also unsubscribe"`
}

type unsubscribeInput struct {
	Senders []string `json:"senders" jsonschema:"addresses or @domains"`
	Then    string   `json:"then,omitempty" jsonschema:"erase|trash their mail too"`
}

type markInput struct {
	IDs              []int64 `json:"ids" jsonschema:"message ids from find"`
	Action           string  `json:"action" jsonschema:"read|unread|flag|unflag|move"`
	MoveTo           string  `json:"move_to,omitempty" jsonschema:"target mailbox for move"`
	IncludeProtected []int64 `json:"include_protected,omitempty" jsonschema:"protected ids to move anyway"`
}

type executeInput struct {
	PlanID string `json:"plan_id" jsonschema:"from a preview"`
}

func (h *handlers) addWrite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "cleanup", Description: "Preview a bulk erase, trash, mark-read or move.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in cleanupInput) (*mcp.CallToolResult, any, error) {
			since, until, err := dateRange(in.Since, in.Until)
			if err != nil {
				return errResult(err)
			}
			f := maildata.Filter{Account: in.Account, From: in.From, Subject: in.Subject, Since: since, Until: until,
				Unread: truthy(in.Unread), IDs: in.IDs}
			if in.Mailbox != "" {
				mailboxFilter(&f, in.Mailbox)
			}
			return h.withEngine(func(eng *actions.Engine) (*mcp.CallToolResult, any, error) {
				p, err := eng.BuildCleanup(ctx, actions.CleanupRequest{Filter: f, Senders: in.Senders, Action: in.Action,
					MoveTo: in.MoveTo, Unsubscribe: in.Unsubscribe, AllowProtected: in.IncludeProtected})
				if err != nil {
					return errResult(err)
				}
				if in.Action == "erase" {
					h.erasePlans.add(p.PlanID)
				}
				if in.Unsubscribe {
					return textResult(renderUnsub(p))
				}
				return textResult(renderPreview(p))
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "unsubscribe", Description: "Preview unsubscribing from senders.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in unsubscribeInput) (*mcp.CallToolResult, any, error) {
			return h.withEngine(func(eng *actions.Engine) (*mcp.CallToolResult, any, error) {
				p, err := eng.BuildUnsubscribe(ctx, in.Senders, in.Then)
				if err != nil {
					return errResult(err)
				}
				if in.Then == "erase" {
					h.erasePlans.add(p.PlanID)
				}
				return textResult(renderUnsub(p))
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "mark", Description: "Mark messages read/unread/flagged, or move them.", Annotations: markHints},
		func(ctx context.Context, _ *mcp.CallToolRequest, in markInput) (*mcp.CallToolResult, any, error) {
			return h.withEngine(func(eng *actions.Engine) (*mcp.CallToolResult, any, error) {
				p, now, err := eng.BuildMark(ctx, actions.MarkRequest{IDs: in.IDs, Action: in.Action, MoveTo: in.MoveTo,
					AllowProtected: in.IncludeProtected})
				if err != nil {
					return errResult(err)
				}
				if now && p.Result != nil {
					r := *p.Result
					var b strings.Builder
					if r.NotFound == 0 && r.Failed == 0 {
						fmt.Fprintf(&b, "done: %d", r.Done)
					} else {
						b.WriteString(renderReport(r, ""))
					}
					appendExtras(&b, p)
					return textResult(b.String())
				}
				return textResult(renderPreview(p))
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "execute", Description: "Run a previewed plan after the user confirms in a macOS dialog.", Annotations: execHints},
		func(ctx context.Context, _ *mcp.CallToolRequest, in executeInput) (*mcp.CallToolResult, any, error) {
			return h.withEngine(func(eng *actions.Engine) (*mcp.CallToolResult, any, error) {
				erase := h.erasePlans.take(in.PlanID)
				r, err := eng.Execute(ctx, in.PlanID)
				if err != nil {
					return errResult(err)
				}
				dir := ""
				if erase && eng.Archive != nil && r.Done > 0 {
					dir = eng.Archive.Dir
				}
				return textResult(renderReport(r, dir))
			})
		})
}

func renderReport(r actions.Report, archiveDir string) string {
	if r.Cancelled {
		return "cancelled"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "done: %d, not found: %d, failed: %d", r.Done, r.NotFound, r.Failed)
	for _, l := range r.PerAccount {
		b.WriteString("\n" + oneLine(l))
	}
	for _, l := range r.Unsubscribe {
		b.WriteString("\n" + oneLine(l))
	}
	if archiveDir != "" {
		b.WriteString("\narchive: " + archiveDir)
	}
	return b.String()
}

func appendExtras(b *strings.Builder, p actions.Preview) {
	if n := len(p.Protected); n > 0 {
		fmt.Fprintf(b, "\nprotected (kept): %d", n)
		for i, m := range p.Protected {
			if i == maxProtRows {
				fmt.Fprintf(b, "\n… %d more", n-i)
				break
			}
			fmt.Fprintf(b, "\n%d | %s | %s | %s", m.ID, clipRunes(oneLine(m.From), 60), clipRunes(oneLine(m.Subject), 80), m.Reason)
		}
	}
	for _, s := range p.Skipped {
		b.WriteString("\nskipped: " + oneLine(s))
	}
}

func renderPreview(p actions.Preview) string {
	var b strings.Builder
	if p.PlanID == "" {
		b.WriteString("nothing to do")
		appendExtras(&b, p)
		return b.String()
	}
	fmt.Fprintf(&b, "plan: %s\n%d messages from %d senders", p.PlanID, p.Count, len(p.BySender))
	for i, s := range p.BySender {
		if i == maxSenderRows {
			fmt.Fprintf(&b, "\n… %d more senders", len(p.BySender)-i)
			break
		}
		fmt.Fprintf(&b, "\n%d | %s", s.Count, oneLine(s.Sender))
	}
	appendExtras(&b, p)
	fmt.Fprintf(&b, "\nconfirm: execute plan_id=%s (a macOS dialog will ask)", p.PlanID)
	return b.String()
}

func renderUnsub(p actions.Preview) string {
	var b strings.Builder
	if p.PlanID != "" {
		fmt.Fprintf(&b, "plan: %s\n", p.PlanID)
	}
	for i, u := range p.Unsub {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s | %s", oneLine(u.Sender), u.Method)
	}
	if p.PlanID != "" && p.Count > 0 {
		fmt.Fprintf(&b, "\n%d messages from %d senders will also be cleaned up", p.Count, len(p.BySender))
	}
	appendExtras(&b, p)
	if p.PlanID != "" {
		fmt.Fprintf(&b, "\nconfirm: execute plan_id=%s (a macOS dialog will ask)", p.PlanID)
	}
	return b.String()
}

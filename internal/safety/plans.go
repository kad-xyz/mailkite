package safety

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PlanTTL is how long a plan can be executed after it was made.
const PlanTTL = 10 * time.Minute

var (
	ErrPlanMismatch = errors.New("plan does not match its targets; make a new preview")
	ErrPlanUnknown  = errors.New("plan not found or already used; make a new preview")
	ErrPlanExpired  = errors.New("plan expired (older than 10 minutes); make a new preview")
)

// Target is one message a plan acts on.
type Target struct {
	ID            int64 // Envelope Index ROWID
	MessageID     string
	Mailbox       string
	UID           uint32
	From, Subject string
}

// Plan is a previewed action waiting for the user's confirmation.
type Plan struct {
	ID, Action string
	Account    map[string][]Target // Mail account UUID -> targets
	Hash       string              // PlanHash of the action and its Message-IDs, or of exact content
	Created    time.Time
	Summary    string // dialog body
	Button     string // dialog action button, e.g. "Стереть 214"
	Extra      any    // action-specific payload
}

// Plans holds plans in process memory only. The zero value is ready to use.
type Plans struct {
	mu sync.Mutex
	m  map[string]Plan
}

func newID() string {
	var b [16]byte
	rand.Read(b[:]) // never fails (crypto/rand panics instead)
	return hex.EncodeToString(b[:])
}

// New stores p under a fresh random ID and returns it. Created defaults to now.
// Plans older than PlanTTL by the wall clock are dropped here, so memory stays
// bounded without timers.
func (ps *Plans) New(p Plan) string {
	now := time.Now()
	if p.Created.IsZero() {
		p.Created = now
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.m == nil {
		ps.m = map[string]Plan{}
	}
	for id, old := range ps.m {
		if now.Sub(old.Created) > PlanTTL {
			delete(ps.m, id)
		}
	}
	id := newID()
	for _, dup := ps.m[id]; dup; _, dup = ps.m[id] {
		id = newID()
	}
	p.ID = id
	ps.m[id] = p
	return id
}

// Len reports how many plans are stored.
func (ps *Plans) Len() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.m)
}

// Take returns the plan and removes it, so a plan runs at most once. An expired
// plan is removed too and reported as ErrPlanExpired.
func (ps *Plans) Take(id string, now time.Time) (Plan, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.m[id]
	if !ok {
		return Plan{}, ErrPlanUnknown
	}
	delete(ps.m, id)
	if now.Sub(p.Created) > PlanTTL {
		return Plan{}, ErrPlanExpired
	}
	return p, nil
}

// PlanHash is the SHA-256 (hex) of the action and the sorted Message-IDs. Every
// part is length-prefixed, so different lists never encode the same way.
func PlanHash(action string, messageIDs []string) string {
	ids := append([]string(nil), messageIDs...)
	sort.Strings(ids)
	h := sha256.New()
	put := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	put(action)
	for _, id := range ids {
		put(id)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// targetIDs returns every Message-ID of targets; an empty one is an error, since a
// plan must name exactly the messages it acts on.
func targetIDs(targets map[string][]Target) ([]string, error) {
	var ids []string
	for acc, ts := range targets {
		for _, t := range ts {
			if strings.TrimSpace(t.MessageID) == "" {
				return nil, fmt.Errorf("plan: account %s: target %d has no Message-ID", acc, t.ID)
			}
			ids = append(ids, t.MessageID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("plan: no targets")
	}
	return ids, nil
}

// NewPlanFor builds a plan bound to its targets: Hash is PlanHash of the action and
// all targets' Message-IDs. Targets without a Message-ID are refused.
func NewPlanFor(action string, targets map[string][]Target, summary, button string, extra any, now time.Time) (Plan, error) {
	ids, err := targetIDs(targets)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Action: action, Account: targets, Hash: PlanHash(action, ids), Created: now,
		Summary: summary, Button: button, Extra: extra}, nil
}

// Verify recomputes the hash for action and targets and fails unless it equals the
// plan's, so a plan can only run the action on the messages it was made for.
func (p Plan) Verify(action string, targets map[string][]Target) error {
	if action != p.Action {
		return ErrPlanMismatch
	}
	ids, err := targetIDs(targets)
	if err != nil {
		return err
	}
	if p.Hash == "" || PlanHash(action, ids) != p.Hash {
		return ErrPlanMismatch
	}
	return nil
}

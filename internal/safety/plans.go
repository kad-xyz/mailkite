package safety

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// PlanTTL is how long a plan can be executed after it was made.
const PlanTTL = 10 * time.Minute

var (
	ErrPlanUnknown = errors.New("plan not found or already used; make a new preview")
	ErrPlanExpired = errors.New("plan expired (older than 10 minutes); make a new preview")
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
// Plans expired relative to p.Created are dropped here, so memory stays bounded
// without timers.
func (ps *Plans) New(p Plan) string {
	if p.Created.IsZero() {
		p.Created = time.Now()
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.m == nil {
		ps.m = map[string]Plan{}
	}
	for id, old := range ps.m {
		if p.Created.Sub(old.Created) > PlanTTL {
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

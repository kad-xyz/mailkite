package safety

import (
	"errors"
	"regexp"
	"testing"
	"time"
)

func TestPlansTakeOnceAndExpiry(t *testing.T) {
	var ps Plans
	now := time.Now()
	id := ps.New(Plan{Action: "erase", Created: now, Button: "Стереть 2"})
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("id %q", id)
	}
	if id2 := ps.New(Plan{Action: "erase", Created: now}); id2 == id {
		t.Fatal("ids repeat")
	}
	p, err := ps.Take(id, now.Add(10*time.Minute))
	if err != nil || p.ID != id || p.Action != "erase" || p.Button != "Стереть 2" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := ps.Take(id, now); !errors.Is(err, ErrPlanUnknown) {
		t.Fatalf("second take: %v", err)
	}
	old := ps.New(Plan{Action: "erase", Created: now})
	if _, err := ps.Take(old, now.Add(11*time.Minute)); !errors.Is(err, ErrPlanExpired) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := ps.Take(old, now); !errors.Is(err, ErrPlanUnknown) {
		t.Fatalf("expired plan must be removed: %v", err)
	}
	if _, err := ps.Take("nope", now); !errors.Is(err, ErrPlanUnknown) {
		t.Fatal(err)
	}
}

func TestPlanHashOrderIndependent(t *testing.T) {
	a := PlanHash("erase", []string{"b@x", "a@x"})
	if a != PlanHash("erase", []string{"a@x", "b@x"}) || len(a) != 64 {
		t.Fatal(a)
	}
	if a == PlanHash("move", []string{"a@x", "b@x"}) || a == PlanHash("erase", []string{"a@x"}) {
		t.Fatal("hash must cover action and ids")
	}
	if PlanHash("erase", []string{"a", "b"}) == PlanHash("erase", []string{"a\nb"}) {
		t.Fatal("ambiguous encoding")
	}
}

func TestPlansNewDropsExpired(t *testing.T) {
	var ps Plans
	now := time.Now()
	ps.New(Plan{Created: now.Add(-11 * time.Minute)}) // expired by the wall clock
	ps.New(Plan{Created: now.Add(-11 * time.Minute)}) // pruned by the next New
	if len(ps.m) != 1 {
		t.Fatalf("expired plan not pruned by wall clock: %d plans", len(ps.m))
	}
	fresh := ps.New(Plan{Created: now.Add(-time.Minute)})
	ps.New(Plan{}) // Created defaults to time.Now()
	if len(ps.m) != 2 {
		t.Fatalf("%d plans kept", len(ps.m))
	}
	if _, ok := ps.m[fresh]; !ok {
		t.Fatal("fresh plan pruned")
	}
}

func TestNewPlanForAndVerify(t *testing.T) {
	now := time.Now()
	targets := map[string][]Target{
		"U1": {{MessageID: "b@x", Mailbox: "INBOX", UID: 2}, {MessageID: "a@x", Mailbox: "INBOX", UID: 1}},
		"U2": {{MessageID: "c@x", Mailbox: "INBOX", UID: 9}},
	}
	p, err := NewPlanFor("erase", targets, "3 письма", "Стереть 3", 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Action != "erase" || p.Summary != "3 письма" || p.Button != "Стереть 3" || p.Extra != 42 || !p.Created.Equal(now) || len(p.Account) != 2 {
		t.Fatalf("%+v", p)
	}
	if p.Hash != PlanHash("erase", []string{"c@x", "a@x", "b@x"}) {
		t.Fatalf("hash %s", p.Hash)
	}
	same := map[string][]Target{
		"U2": {{MessageID: "c@x"}},
		"U1": {{MessageID: "a@x"}, {MessageID: "b@x"}},
	}
	if err := p.Verify("erase", same); err != nil {
		t.Fatalf("verify same targets: %v", err)
	}
	if err := p.Verify("move", same); err == nil {
		t.Fatal("other action must fail")
	}
	extra := map[string][]Target{"U1": {{MessageID: "a@x"}, {MessageID: "b@x"}, {MessageID: "d@x"}}, "U2": {{MessageID: "c@x"}}}
	if err := p.Verify("erase", extra); err == nil {
		t.Fatal("changed targets must fail")
	}
	if err := p.Verify("erase", map[string][]Target{"U1": {{MessageID: "a@x"}, {MessageID: "b@x"}, {MessageID: ""}}, "U2": {{MessageID: "c@x"}}}); err == nil {
		t.Fatal("empty Message-ID must fail verify")
	}
	if _, err := NewPlanFor("erase", map[string][]Target{"U1": {{MessageID: "a@x"}, {MessageID: " "}}}, "", "Стереть 2", nil, now); err == nil {
		t.Fatal("empty Message-ID must be rejected")
	}
	if _, err := NewPlanFor("erase", nil, "", "Стереть 0", nil, now); err == nil {
		t.Fatal("no targets must be rejected")
	}
}

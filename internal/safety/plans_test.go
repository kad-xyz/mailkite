package safety

import (
	"errors"
	"regexp"
	"testing"
	"time"
)

func TestPlansTakeOnceAndExpiry(t *testing.T) {
	var ps Plans
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
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
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ps.New(Plan{Created: now})
	ps.New(Plan{Created: now.Add(11 * time.Minute)})
	if len(ps.m) != 1 {
		t.Fatalf("%d plans kept", len(ps.m))
	}
}

package fts

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/testfixture"
)

var ctx = context.Background()

func setup(t *testing.T) (*testfixture.Fixture, *maildata.Store, *Index) {
	t.Helper()
	f := testfixture.New(t)
	f.AddAccount("A", "Work", "IMAP")
	st, err := maildata.Open(maildata.Options{MailRoot: f.Root, AccountsDB: f.AccountsDB})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ix, err := Open(filepath.Join(t.TempDir(), "sub", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return f, st, ix
}

func add(f *testfixture.Fixture, subj, body string) int64 {
	raw := "From: a@x.com\r\nSubject: " + subj + "\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
	return f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "a@x.com", Subject: subj, Raw: []byte(raw)})
}

func search(t *testing.T, ix *Index, q string) []int64 {
	t.Helper()
	ids, err := ix.Search(ctx, q, 0)
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	return ids
}

func TestSyncAndSearchBody(t *testing.T) {
	f, st, ix := setup(t)
	id := add(f, "hello", "заберите копию трудовой книжки")
	id2 := add(f, "tree", "наша елка")
	id3 := add(f, "tree2", "Ёлка во дворе")
	if _, _, err := ix.Sync(ctx, st); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"трудовой", "ТРУДОВ"} {
		if got := search(t, ix, q); !slices.Equal(got, []int64{id}) {
			t.Errorf("%q = %v", q, got)
		}
	}
	for _, q := range []string{"ёлка", "елка"} {
		got := search(t, ix, q)
		slices.Sort(got)
		if !slices.Equal(got, []int64{id2, id3}) {
			t.Errorf("%q = %v", q, got)
		}
	}
}

func TestSyncIncremental(t *testing.T) {
	f, st, ix := setup(t)
	add(f, "one", "body")
	if a, _, err := ix.Sync(ctx, st); err != nil || a != 1 {
		t.Fatalf("first: %d %v", a, err)
	}
	if a, _, err := ix.Sync(ctx, st); err != nil || a != 0 {
		t.Fatalf("second: %d %v", a, err)
	}
	add(f, "two", "body")
	if a, _, err := ix.Sync(ctx, st); err != nil || a != 1 {
		t.Fatalf("third: %d %v", a, err)
	}
}

func TestSyncPrune(t *testing.T) {
	f, st, ix := setup(t)
	id := add(f, "gone", "uniqueword")
	add(f, "stay", "other")
	if _, _, err := ix.Sync(ctx, st); err != nil {
		t.Fatal(err)
	}
	f.RemoveMessage(id)
	if _, r, _ := ix.Sync(ctx, st); r != 0 {
		t.Fatalf("pruned too early: %d", r)
	}
	old := time.Now().Add(-2 * time.Minute).Unix()
	if _, err := ix.db.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES('last_prune',?)", old); err != nil {
		t.Fatal(err)
	}
	if _, r, err := ix.Sync(ctx, st); err != nil || r != 1 {
		t.Fatalf("removed=%d err=%v", r, err)
	}
	if got := search(t, ix, "uniqueword"); len(got) != 0 {
		t.Errorf("still found: %v", got)
	}
}

func TestSearchEscapes(t *testing.T) {
	f, st, ix := setup(t)
	add(f, "x", "a b")
	ix.Sync(ctx, st)
	for _, q := range []string{`"a OR b) NEAR(`, `( ) * "`, `   `, ``, `-x AND`, `a"b`} {
		if _, err := ix.Search(ctx, q, 10); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func TestRankingAndRebuild(t *testing.T) {
	f, st, ix := setup(t)
	weak := add(f, "s", "invoice and many other words here to dilute the match a lot more")
	strong := add(f, "invoice invoice", "invoice")
	if _, _, err := ix.Sync(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got := search(t, ix, "invoice"); len(got) != 2 || got[0] != strong || got[1] != weak {
		t.Errorf("rank: %v", got)
	}
	if err := ix.Rebuild(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got := search(t, ix, "invoice"); len(got) != 2 {
		t.Errorf("after rebuild: %v", got)
	}
}

func TestSyncSpeed(t *testing.T) {
	f, st, ix := setup(t)
	for i := 0; i < 100; i++ {
		add(f, "s", "some body text")
	}
	t0 := time.Now()
	a, _, err := ix.Sync(ctx, st)
	if err != nil || a != 100 {
		t.Fatal(a, err)
	}
	t.Logf("100 messages: %v", time.Since(t0))
}

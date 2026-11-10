package fts

import (
	"context"
	"os"
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
	if _, r, err := ix.Sync(ctx, st); err != nil || r != 0 {
		t.Fatalf("pruned too early: %d %v", r, err)
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
	if _, _, err := ix.Sync(ctx, st); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]int{`"a OR b) NEAR(`: 0, `( ) * "`: 0, `   `: 0, ``: 0, `-x AND`: 0, `a"b`: 1, `a b`: 1, `a OR`: 0, `NEAR(a`: 0} {
		got, err := ix.Search(ctx, q, 10)
		if err != nil || len(got) != want {
			t.Errorf("%q: got %v err %v, want %d results", q, got, err, want)
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
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("too slow: %v", d)
	}
	t.Logf("100 messages: %v", time.Since(t0))
}

func mustSync(t *testing.T, ix *Index, st *maildata.Store) (int, int) {
	t.Helper()
	a, r, err := ix.Sync(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	return a, r
}

func pendingCount(t *testing.T, ix *Index) (n int) {
	t.Helper()
	if err := ix.db.QueryRow("SELECT count(*) FROM pending").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSearchFields(t *testing.T) {
	f, st, ix := setup(t)
	id := f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "boss@corp.example", FromName: "Ёжик Петров",
		Subject: "Квартальный отчёт", To: []string{"team@dest.example"}, Attachments: []string{"budget2026.xlsx"}})
	other := add(f, "unrelated", "nothing")
	mustSync(t, ix, st)
	for _, q := range []string{"квартальный", "ежик", "петров", "boss@corp.example", "corp", "team@dest.example", "budget2026", "xlsx"} {
		if got := search(t, ix, q); !slices.Equal(got, []int64{id}) {
			t.Errorf("%q = %v (other=%d)", q, got, other)
		}
	}
}

func TestPartialEmlxIndexedNotPending(t *testing.T) {
	f, st, ix := setup(t)
	raw := []byte("From: a@x.com\r\nSubject: p\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\npartialword\r\n")
	id := f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "a@x.com", Subject: "p", Raw: raw, Partial: true})
	mustSync(t, ix, st)
	if got := search(t, ix, "partialword"); !slices.Equal(got, []int64{id}) {
		t.Errorf("partial body not indexed: %v", got)
	}
	if n := pendingCount(t, ix); n != 0 {
		t.Errorf("partial message must not stay pending: %d", n)
	}
}

func TestMissingEmlxRetried(t *testing.T) {
	f, st, ix := setup(t)
	id := add(f, "late", "latebodyword")
	sm, mb, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	p, _, ok := st.EmlxPath(sm.ID, mb)
	if !ok {
		t.Fatal("no emlx")
	}
	data, _ := os.ReadFile(p)
	os.Remove(p)
	mustSync(t, ix, st)
	if got := search(t, ix, "late"); !slices.Equal(got, []int64{id}) {
		t.Errorf("subject search: %v", got)
	}
	if got := search(t, ix, "latebodyword"); len(got) != 0 || pendingCount(t, ix) != 1 {
		t.Errorf("body found without emlx: %v pending=%d", got, pendingCount(t, ix))
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	agePending(t, ix)
	if a, _ := mustSync(t, ix, st); a != 0 {
		t.Errorf("added=%d", a)
	}
	if got := search(t, ix, "latebodyword"); !slices.Equal(got, []int64{id}) {
		t.Errorf("body not reindexed: %v", got)
	}
	if pendingCount(t, ix) != 0 {
		t.Errorf("pending not cleared")
	}
}

func TestPendingDroppedOnPrune(t *testing.T) {
	f, st, ix := setup(t)
	id := f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "a@x.com", Subject: "p", Partial: true})
	mustSync(t, ix, st)
	f.RemoveMessage(id)
	agePending(t, ix)
	mustSync(t, ix, st) // retry sees ErrNotFound
	if pendingCount(t, ix) != 0 {
		t.Errorf("pending not dropped")
	}
}

func TestDeletedNotIndexed(t *testing.T) {
	f, st, ix := setup(t)
	f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://A/INBOX", From: "a@x.com", Subject: "ghost", Deleted: true})
	if a, _ := mustSync(t, ix, st); a != 0 {
		t.Errorf("added=%d", a)
	}
	if got := search(t, ix, "ghost"); len(got) != 0 {
		t.Errorf("found deleted: %v", got)
	}
}

func TestChunkedSyncKeepsProgress(t *testing.T) {
	f, st, ix := setup(t)
	for i := 0; i < 5; i++ {
		add(f, "chunky", "body")
	}
	oldChunk, oldHook := chunkSize, afterChunk
	defer func() { chunkSize, afterChunk = oldChunk, oldHook }()
	chunkSize = 2
	cctx, cancel := context.WithCancel(ctx)
	afterChunk = cancel
	if _, _, err := ix.Sync(cctx, st); err == nil {
		t.Fatal("expected cancellation error")
	}
	if got := search(t, ix, "chunky"); len(got) != 2 {
		t.Fatalf("committed chunk lost: %v", got)
	}
	afterChunk = nil
	if a, _ := mustSync(t, ix, st); a != 3 {
		t.Errorf("resumed added=%d, want 3", a)
	}
	if got := search(t, ix, "chunky"); len(got) != 5 {
		t.Errorf("total %v", got)
	}
}

func TestIndexFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d", "i.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v", p, fi, err)
		}
	}
}

func TestPendingRetryRotates(t *testing.T) {
	f, st, ix := setup(t)
	oldBatch := pendingBatch
	defer func() { pendingBatch = oldBatch }()
	pendingBatch = 2
	for i := 0; i < 3; i++ {
		fid := add(f, "p", "x")
		fsm, fmb, _ := st.Get(ctx, fid)
		fp, _, _ := st.EmlxPath(fsm.ID, fmb)
		os.Remove(fp) // no emlx: stays pending
	}
	late := add(f, "late", "rotateword")
	sm, mb, _ := st.Get(ctx, late)
	p, _, _ := st.EmlxPath(sm.ID, mb)
	data, _ := os.ReadFile(p)
	os.Remove(p)
	mustSync(t, ix, st) // retries the first two, stamps them
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	mustSync(t, ix, st) // must reach the untried rows, including the late one
	if got := search(t, ix, "rotateword"); !slices.Equal(got, []int64{late}) {
		t.Errorf("late body starved: %v", got)
	}
}

func agePending(t *testing.T, ix *Index) {
	t.Helper()
	if _, err := ix.db.Exec("UPDATE pending SET retried_at=0"); err != nil {
		t.Fatal(err)
	}
}

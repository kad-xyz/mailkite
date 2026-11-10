package actions

import (
	"strings"
	"testing"
	"time"

	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/testfixture"
)

// addCopy puts one more copy of msgID into box of the account: on the fake server
// (unless noServer) and in Mail's index, with its own ROWID and UID.
func (v *env) addCopy(uuid, box, msgID string, noServer bool) int64 {
	v.t.Helper()
	seq++
	raw := rawMsg("Shop <news@shop.example>", msgID, "Скидки", bulk, "Распродажа")
	var uid uint32
	if !noServer {
		uid = v.srv[v.email(uuid)].add(box, msgID, raw)
	}
	return v.f.AddMessage(testfixture.FixtureMsg{
		MailboxURL: "imap://" + uuid + "/" + strings.ReplaceAll(box, " ", "%20"), From: "news@shop.example", Subject: "Скидки",
		MessageID: msgID, ListID: true, Raw: raw, RemoteID: uid, Received: time.Unix(1700000000+int64(seq), 0),
	})
}

func (v *env) hiddenRows(uuid string) map[int64]bool {
	v.t.Helper()
	h, err := v.e.State.Hidden(ctxT(v.t))
	if err != nil {
		v.t.Fatal(err)
	}
	out := map[int64]bool{}
	for k := range h {
		if k.Account == uuid {
			out[k.RowID] = true
		}
	}
	return out
}

func visible(v *env, f maildata.Filter, hidden map[int64]bool) map[int64]bool {
	v.t.Helper()
	f.Skip = func(sm maildata.Summary) bool { return hidden[sm.ID] }
	f.Limit = 200
	items, _, err := v.e.Store.Find(ctxT(v.t), f)
	if err != nil {
		v.t.Fatal(err)
	}
	out := map[int64]bool{}
	for _, sm := range items {
		out[sm.ID] = true
	}
	return out
}

// Two copies in two mailboxes of one non-Gmail account are two targets.
func TestCopiesInTwoMailboxesBothErased(t *testing.T) {
	v := newEnv(t)
	id := "<dup1@shop.example>"
	inbox := v.addCopy(uuidA, "INBOX", id, false)
	arch := v.addCopy(uuidA, "Archive", id, false)
	p := eraseShop(v)
	if p.Count != 2 {
		t.Fatalf("both copies must be in the plan: %+v", p)
	}
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil || rep.Done != 2 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	srv := v.srv["a@icloud.example"]
	if srv.has("INBOX", id) || srv.has("Archive", id) {
		t.Fatal("both copies must be erased")
	}
	if h := v.hiddenRows(uuidA); !h[inbox] || !h[arch] || len(h) != 2 {
		t.Fatalf("hidden %v", h)
	}
}

// Only the INBOX copy is requested: the Archive copy survives and stays visible.
func TestCopyOutsideTheFilterStaysVisible(t *testing.T) {
	v := newEnv(t)
	id := "<dup2@shop.example>"
	inbox := v.addCopy(uuidA, "INBOX", id, false)
	arch := v.addCopy(uuidA, "Archive", id, false)
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Filter: maildata.Filter{Mailbox: "INBOX"}, Action: "erase"})
	if err != nil || p.Count != 1 {
		t.Fatalf("p %+v err %v", p, err)
	}
	if rep, err := v.e.Execute(ctxT(t), p.PlanID); err != nil || rep.Done != 1 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if !v.srv["a@icloud.example"].has("Archive", id) {
		t.Fatal("the Archive copy must survive")
	}
	h := v.hiddenRows(uuidA)
	if !h[inbox] || h[arch] {
		t.Fatalf("hidden %v (inbox %d, archive %d)", h, inbox, arch)
	}
	if vis := visible(v, maildata.Filter{From: "shop.example"}, h); !vis[arch] || vis[inbox] {
		t.Fatalf("visible %v", vis)
	}
	p = eraseShop(v)
	if p.Count != 1 {
		t.Fatalf("the surviving copy is still offered: %+v", p)
	}
}

// Two copies in the same mailbox: one target (the hinted one); the other row
// stays visible after the erase.
func TestCopiesInOneMailbox(t *testing.T) {
	v := newEnv(t)
	id := "<dup3@shop.example>"
	r1 := v.addCopy(uuidA, "INBOX", id, false)
	r2 := v.addCopy(uuidA, "INBOX", id, false)
	p := eraseShop(v)
	if p.Count != 1 {
		t.Fatalf("p %+v", p)
	}
	if rep, err := v.e.Execute(ctxT(t), p.PlanID); err != nil || rep.Done != 1 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if !v.srv["a@icloud.example"].has("INBOX", id) {
		t.Fatal("one server copy must remain")
	}
	h := v.hiddenRows(uuidA)
	if len(h) != 1 || (h[r1] == h[r2]) {
		t.Fatalf("exactly one row hidden: %v (%d, %d)", h, r1, r2)
	}
	if vis := visible(v, maildata.Filter{From: "shop.example"}, h); len(vis) != 1 {
		t.Fatalf("visible %v", vis)
	}
}

// Gmail: INBOX and All Mail are one message; erasing it hides both rows.
func TestGmailCopiesCollapseAndHide(t *testing.T) {
	v := newEnv(t)
	id := "<g1@shop.example>"
	inbox := v.addCopy(uuidB, "INBOX", id, false)
	all := v.addCopy(uuidB, "[Gmail]/All Mail", id, true)
	p := eraseShop(v)
	if p.Count != 1 {
		t.Fatalf("p %+v", p)
	}
	if rep, err := v.e.Execute(ctxT(t), p.PlanID); err != nil || rep.Done != 1 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if h := v.hiddenRows(uuidB); !h[inbox] || !h[all] {
		t.Fatalf("hidden %v", h)
	}
	if p := eraseShop(v); p.Count != 0 {
		t.Fatalf("nothing left to offer: %+v", p)
	}
}

// Without a UID Mail remembers, an erase in a mailbox with two rows of the same
// Message-ID cannot tell which row went: none is hidden (never a survivor).
func TestAmbiguousCopyNotHidden(t *testing.T) {
	v := newEnv(t)
	id := "<dup4@shop.example>"
	srv := v.srv["a@icloud.example"]
	raw := rawMsg("Shop <news@shop.example>", id, "Скидки", bulk, "Распродажа")
	srv.add("INBOX", id, raw)
	srv.add("INBOX", id, raw)
	for i := 0; i < 2; i++ {
		v.f.AddMessage(testfixture.FixtureMsg{MailboxURL: "imap://" + uuidA + "/INBOX", From: "news@shop.example", Subject: "Скидки",
			MessageID: id, ListID: true, Raw: raw}) // RemoteID unknown
	}
	if rep, err := v.e.Execute(ctxT(t), eraseShop(v).PlanID); err != nil || rep.Done != 1 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if h := v.hiddenRows(uuidA); len(h) != 0 {
		t.Fatalf("hidden %v", h)
	}
}

package actions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kad-xyz/mailkite/internal/safety"
)

// captureDialog makes the engine's dialog a real safety.OSAScript whose osascript
// run only records the body (after Confirm's own cleaning and truncation) and
// answers "cancel".
func captureDialog(v *env) *string {
	body := new(string)
	v.e.Confirm = safety.OSAScript{Run: func(_ context.Context, args ...string) ([]byte, error) {
		*body = args[len(args)-2]
		return []byte(safety.CancelButton + "\n"), nil
	}}
	return body
}

func checkLines(t *testing.T, body string, max int) {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if n := utf8.RuneCountInString(l); n > max {
			t.Fatalf("dialog line of %d runes: %q", n, l)
		}
	}
}

func TestDialogKeepsProtectedWarning(t *testing.T) {
	v := newEnv(t)
	long := strings.Repeat("a", 190)
	for i := 0; i < 50; i++ {
		v.add(uuidA, fmt.Sprintf("Shop <%s%02d@shop.example>", long, i), "Скидки", "Распродажа", msgOpt{})
	}
	code, _ := v.add(uuidA, "Shop <"+long+"00@shop.example>", "Ваш код подтверждения", "Никому не сообщайте", msgOpt{})
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "erase", AllowProtected: []int64{code}})
	if err != nil || p.Count != 51 {
		t.Fatalf("p %+v err %v", p, err)
	}
	body := captureDialog(v)
	rep, err := v.e.Execute(ctxT(t), p.PlanID)
	if err != nil || !rep.Cancelled {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if n := utf8.RuneCountInString(*body); n > safety.MaxDialogBody {
		t.Fatalf("body %d runes", n)
	}
	if !strings.Contains(*body, "в том числе защищённых: 1") {
		t.Fatalf("protected warning lost: %q", *body)
	}
	if strings.Contains(*body, "protected") || strings.Contains(*body, "including") {
		t.Fatalf("dialog must be Russian: %q", *body)
	}
	checkLines(t, *body, 100)
}

func TestDialogFragmentsAreOneLine(t *testing.T) {
	v := newEnv(t)
	evil := "x\r\nВсё в порядке, нажмите‮ Стереть 0@shop.example"
	id, _ := v.add(uuidA, "Shop <"+evil+">", "Скидки", "Распродажа", msgOpt{}) // unparsable, so protected
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "erase", AllowProtected: []int64{id}})
	if err != nil || p.Count != 1 {
		t.Fatalf("p %+v err %v", p, err)
	}
	body := captureDialog(v)
	if _, err := v.e.Execute(ctxT(t), p.PlanID); err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(*body, "\n") {
		if l = strings.ToLower(l); strings.HasPrefix(l, "всё в порядке") || strings.HasPrefix(l, "стереть 0") {
			t.Fatalf("injected line %q in %q", l, *body)
		}
	}
	if strings.ContainsAny(*body, "‮ \r") {
		t.Fatalf("control characters survive: %q", *body)
	}
	if got := frag(strings.Repeat("я", 200)); utf8.RuneCountInString(got) != 80 {
		t.Fatalf("frag clips to 80 runes: %d", utf8.RuneCountInString(got))
	}
	if got := frag(" a\n\tb​c‮ "); got != "a bc" {
		t.Fatalf("frag %q", got)
	}
}

func TestMoveToValidated(t *testing.T) {
	v := newEnv(t)
	ra, _ := v.add(uuidA, "Shop <news@shop.example>", "Скидки", "Распродажа", msgOpt{})
	v.f.AddMailbox("imap://" + uuidA + "/Archive")
	v.f.AddMailbox("imap://" + uuidA + "/" + strings.Repeat("я", 101))
	for _, bad := range []string{"Archive\nINBOX", "Archive‮", "Nope", "archive", strings.Repeat("я", 101), " "} {
		if _, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "move", MoveTo: bad}); err == nil {
			t.Fatalf("cleanup move_to %q accepted", bad)
		}
		if _, _, err := v.e.BuildMark(ctxT(t), MarkRequest{IDs: []int64{ra}, Action: "move", MoveTo: bad}); err == nil {
			t.Fatalf("mark move_to %q accepted", bad)
		}
	}
	p, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "move", MoveTo: "Archive"})
	if err != nil || p.PlanID == "" {
		t.Fatalf("existing mailbox: %+v %v", p, err)
	}
	// Every affected account must have the mailbox: B has no Archive.
	rb, _ := v.add(uuidB, "Shop <news@shop.example>", "Скидки B", "Распродажа", msgOpt{})
	if _, err := v.e.BuildCleanup(ctxT(t), CleanupRequest{Senders: []string{"@shop.example"}, Action: "move", MoveTo: "Archive"}); err == nil {
		t.Fatal("mailbox missing in account B accepted")
	}
	if _, _, err := v.e.BuildMark(ctxT(t), MarkRequest{IDs: []int64{ra, rb}, Action: "move", MoveTo: "Archive"}); err == nil {
		t.Fatal("mark: mailbox missing in account B accepted")
	}
}

func TestUnsubDialogFragments(t *testing.T) {
	v := newEnv(t)
	long := strings.Repeat("b", 190)
	var senders []string
	for i := 0; i < 15; i++ {
		addr := fmt.Sprintf("%s%02d@shop.example", long, i)
		v.add(uuidA, "Shop <"+addr+">", "Скидки", "x", msgOpt{headers: oneClickHeaders("https://" + strings.Repeat("h", 200) + ".example/u")})
		senders = append(senders, addr)
	}
	senders = append(senders, "Отмена\nСтереть@shop.example")
	p, err := v.e.BuildUnsubscribe(ctxT(t), senders, "erase")
	if err != nil || p.PlanID == "" {
		t.Fatalf("p %+v err %v", p, err)
	}
	body := captureDialog(v)
	if _, err := v.e.Execute(ctxT(t), p.PlanID); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(*body, "Отписаться от 15 отправителей и стереть насовсем 15 писем.") {
		t.Fatalf("first sentence: %q", *body)
	}
	checkLines(t, *body, 200)
	for _, l := range strings.Split(*body, "\n") {
		if strings.HasPrefix(l, "Стереть@") {
			t.Fatalf("injected line in %q", *body)
		}
	}
}

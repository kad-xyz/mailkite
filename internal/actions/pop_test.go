package actions

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestAppleScriptPOPArgs(t *testing.T) {
	var got []string
	var calls int
	p := AppleScriptPOP{Run: func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		got = args
		return []byte("gone\ta@x.example\nleft\tb@x.example\n"), nil
	}}
	evil := `<x"; do shell script "rm -rf ~" --@evil.example>`
	res, err := p.Delete(context.Background(), uuidP, []string{"<a@x.example>", " b@x.example ", evil, ""})
	if err != nil || !slices.Equal(res.Deleted, []string{"<a@x.example>"}) || !slices.Equal(res.Left, []string{" b@x.example "}) || calls != 1 {
		t.Fatalf("result %+v err %v calls %d", res, err, calls)
	}
	i := slices.Index(got, "--")
	if i < 0 {
		t.Fatalf("no -- in %q", got)
	}
	src := got[:i]
	for k, a := range src {
		if k%2 == 0 && a != "-e" {
			t.Fatalf("script args must be -e pairs: %q", src)
		}
		if strings.Contains(a, uuidP) || strings.Contains(a, "x.example") || strings.Contains(a, "rm -rf") {
			t.Fatalf("user data in script source: %q", a)
		}
	}
	want := []string{uuidP, "a@x.example", "b@x.example", `x"; do shell script "rm -rf ~" --@evil.example`}
	if !slices.Equal(got[i+1:], want) {
		t.Fatalf("argv %q want %q", got[i+1:], want)
	}
	script := strings.Join(src, "\n")
	for _, s := range []string{"on run argv", "account id", "message id is", "delete", `"Deleted Messages"`, "trash mailbox",
		"count of (every message of", `"gone" & tab & mid`, `"left" & tab & mid`} {
		if !strings.Contains(script, s) {
			t.Fatalf("script lacks %q", s)
		}
	}
}

func TestAppleScriptPOPNothingAndErrors(t *testing.T) {
	called := false
	p := AppleScriptPOP{Run: func(context.Context, ...string) ([]byte, error) { called = true; return nil, errors.New("boom") }}
	if res, err := p.Delete(context.Background(), uuidP, []string{" ", "<>"}); len(res.Deleted)+len(res.Left) != 0 || err != nil || called {
		t.Fatalf("empty: %+v %v %v", res, err, called)
	}
	if _, err := p.Delete(context.Background(), uuidP, []string{"a@x"}); err == nil {
		t.Fatal("osascript failure must surface")
	}
	if _, err := p.Delete(context.Background(), "", []string{"a@x"}); err == nil {
		t.Fatal("empty account")
	}
	// Duplicates and IDs never asked for are dropped; each requested ID counts once.
	p.Run = func(context.Context, ...string) ([]byte, error) {
		return []byte("gone\ta@x\ngone\ta@x\r\ngone\tzzz@y\n\n"), nil
	}
	if res, err := p.Delete(context.Background(), uuidP, []string{"<a@x>", "b@x"}); err != nil || !slices.Equal(res.Deleted, []string{"<a@x>"}) || len(res.Left) != 0 {
		t.Fatalf("result %+v err %v", res, err)
	}
	// A line without a verified status never counts as deleted; "left" wins over "gone".
	p.Run = func(context.Context, ...string) ([]byte, error) {
		return []byte("a@x\nok b@x\ngone\tc@x\nleft\tc@x\n"), nil
	}
	if res, err := p.Delete(context.Background(), uuidP, []string{"a@x", "b@x", "c@x"}); err != nil || len(res.Deleted) != 0 || !slices.Equal(res.Left, []string{"c@x"}) {
		t.Fatalf("unverified: %+v %v", res, err)
	}
	p.Run = func(context.Context, ...string) ([]byte, error) { return []byte("\n"), nil }
	if res, err := p.Delete(context.Background(), uuidP, []string{"a@x"}); err != nil || len(res.Deleted)+len(res.Left) != 0 {
		t.Fatalf("none: %+v %v", res, err)
	}
}

func TestPOPScriptText(t *testing.T) {
	if s := POPScript(); !strings.HasPrefix(s, "on run argv") || !strings.Contains(s, "\nend run\n") || !strings.HasSuffix(s, "end boxesOf") {
		t.Fatalf("script %q", s)
	}
}

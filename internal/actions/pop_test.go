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
		return []byte("2\n"), nil
	}}
	evil := `<x"; do shell script "rm -rf ~" --@evil.example>`
	n, err := p.Delete(context.Background(), uuidP, []string{"<a@x.example>", " b@x.example ", evil, ""})
	if err != nil || n != 2 || calls != 1 {
		t.Fatalf("n %d err %v calls %d", n, err, calls)
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
	for _, s := range []string{"on run argv", "account id", "message id is", "delete", `"Deleted Messages"`} {
		if !strings.Contains(script, s) {
			t.Fatalf("script lacks %q", s)
		}
	}
}

func TestAppleScriptPOPNothingAndErrors(t *testing.T) {
	called := false
	p := AppleScriptPOP{Run: func(context.Context, ...string) ([]byte, error) { called = true; return nil, errors.New("boom") }}
	if n, err := p.Delete(context.Background(), uuidP, []string{" ", "<>"}); n != 0 || err != nil || called {
		t.Fatalf("empty: %d %v %v", n, err, called)
	}
	if _, err := p.Delete(context.Background(), uuidP, []string{"a@x"}); err == nil {
		t.Fatal("osascript failure must surface")
	}
	if _, err := p.Delete(context.Background(), "", []string{"a@x"}); err == nil {
		t.Fatal("empty account")
	}
	p.Run = func(context.Context, ...string) ([]byte, error) { return []byte("many"), nil }
	if _, err := p.Delete(context.Background(), uuidP, []string{"a@x"}); err == nil {
		t.Fatal("bad output")
	}
	p.Run = func(context.Context, ...string) ([]byte, error) { return []byte("5"), nil }
	if n, _ := p.Delete(context.Background(), uuidP, []string{"a@x"}); n != 1 {
		t.Fatalf("count above request must be clamped: %d", n)
	}
}

func TestPOPScriptText(t *testing.T) {
	if s := POPScript(); !strings.HasPrefix(s, "on run argv") || !strings.HasSuffix(s, "end run") {
		t.Fatalf("script %q", s)
	}
}

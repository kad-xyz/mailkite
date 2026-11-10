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
		return []byte("a@x.example\nb@x.example\n"), nil
	}}
	evil := `<x"; do shell script "rm -rf ~" --@evil.example>`
	del, err := p.Delete(context.Background(), uuidP, []string{"<a@x.example>", " b@x.example ", evil, ""})
	if err != nil || !slices.Equal(del, []string{"<a@x.example>", " b@x.example "}) || calls != 1 {
		t.Fatalf("deleted %q err %v calls %d", del, err, calls)
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
	if del, err := p.Delete(context.Background(), uuidP, []string{" ", "<>"}); len(del) != 0 || err != nil || called {
		t.Fatalf("empty: %q %v %v", del, err, called)
	}
	if _, err := p.Delete(context.Background(), uuidP, []string{"a@x"}); err == nil {
		t.Fatal("osascript failure must surface")
	}
	if _, err := p.Delete(context.Background(), "", []string{"a@x"}); err == nil {
		t.Fatal("empty account")
	}
	// Duplicates and IDs never asked for are dropped; each requested ID counts once.
	p.Run = func(context.Context, ...string) ([]byte, error) { return []byte("a@x\na@x\r\nzzz@y\n\n"), nil }
	if del, err := p.Delete(context.Background(), uuidP, []string{"<a@x>", "b@x"}); err != nil || !slices.Equal(del, []string{"<a@x>"}) {
		t.Fatalf("deleted %q err %v", del, err)
	}
	p.Run = func(context.Context, ...string) ([]byte, error) { return []byte("\n"), nil }
	if del, err := p.Delete(context.Background(), uuidP, []string{"a@x"}); err != nil || len(del) != 0 {
		t.Fatalf("none: %q %v", del, err)
	}
}

func TestPOPScriptText(t *testing.T) {
	if s := POPScript(); !strings.HasPrefix(s, "on run argv") || !strings.HasSuffix(s, "end run") {
		t.Fatalf("script %q", s)
	}
}

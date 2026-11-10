package safety

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"
)

var wantScript = []string{
	"-e", "on run argv",
	"-e", `display dialog (item 2 of argv) with title (item 1 of argv) buttons {"Отмена", item 3 of argv} default button "Отмена" cancel button "Отмена" with icon caution giving up after 120`,
	"-e", "return button returned of result",
	"-e", "end run",
	"--",
}

func TestDialogArgvInjection(t *testing.T) {
	body := "x\" & do shell script \"touch /tmp/p\" & \"\n"
	var got []string
	o := OSAScript{Run: func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("Стереть 2\n"), nil
	}}
	ok, err := o.Confirm(context.Background(), "mailkite", body, "Стереть 2")
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if len(got) != len(wantScript)+3 {
		t.Fatalf("argv %q", got)
	}
	for i, w := range wantScript {
		if got[i] != w {
			t.Fatalf("argv[%d] = %q, want %q", i, got[i], w)
		}
	}
	tail := got[len(wantScript):]
	if tail[0] != "mailkite" || tail[1] != body || tail[2] != "Стереть 2" {
		t.Fatalf("tail %q", tail)
	}
	for _, a := range got[:len(wantScript)] {
		if strings.Contains(a, "touch") {
			t.Fatalf("body leaked into script: %q", a)
		}
	}
}

func TestDialogSanitizesAndTruncates(t *testing.T) {
	var got []string
	o := OSAScript{Run: func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return nil, nil
	}}
	o.Confirm(context.Background(), "t\x00i\ntle", "a\x00b\x07c\r\nd‮e"+strings.Repeat("я", 2000), "Go\x00")
	tail := got[len(got)-3:]
	if tail[0] != "title" {
		t.Fatalf("title %q", tail[0])
	}
	if !strings.HasPrefix(tail[1], "abc\nde") || utf8.RuneCountInString(tail[1]) != 1500 {
		t.Fatalf("body %q… (%d runes)", tail[1][:20], utf8.RuneCountInString(tail[1]))
	}
	if tail[2] != "Go" {
		t.Fatalf("button %q", tail[2])
	}
}

func TestDialogRejectsBadButton(t *testing.T) {
	called := false
	o := OSAScript{Run: func(context.Context, ...string) ([]byte, error) { called = true; return []byte("Отмена\n"), nil }}
	for _, b := range []string{"", "Отмена", "\x00"} {
		if ok, err := o.Confirm(context.Background(), "t", "b", b); ok || err == nil {
			t.Fatalf("button %q: %v %v", b, ok, err)
		}
	}
	if called {
		t.Fatal("osascript must not run")
	}
}

func shExit(script string) error {
	_, err := exec.Command("/bin/sh", "-c", script).Output()
	return err
}

func TestDialogResults(t *testing.T) {
	cancel := shExit(`echo "execution error: User canceled. (-128)" >&2; exit 1`)
	cancel2 := shExit(`echo "0:1: execution error: (-128)" >&2; exit 1`)
	other1 := shExit(`echo "execution error: No user interaction allowed. (-1713)" >&2; exit 1`)
	other2 := shExit(`exit 2`)
	cases := []struct {
		name    string
		out     string
		err     error
		ok      bool
		wantErr bool
	}{
		{"button", "Стереть 3\n", nil, true, false},
		{"button no newline", "Стереть 3", nil, true, false},
		{"other button", "Отмена\n", nil, false, false},
		{"prefix", "Стереть 30\n", nil, false, false},
		{"gave up", "\n", nil, false, false},
		{"empty", "", nil, false, false},
		{"cancel", "", cancel, false, false},
		{"cancel -128", "", cancel2, false, false},
		{"exit 1 other", "", other1, false, true},
		{"exit 2", "", other2, false, true},
		{"exec error", "", errors.New("no such file"), false, true},
		{"button but error", "Стереть 3\n", errors.New("boom"), false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := OSAScript{Run: func(context.Context, ...string) ([]byte, error) { return []byte(c.out), c.err }}
			ok, err := o.Confirm(context.Background(), "t", "b", "Стереть 3")
			if ok != c.ok || (err != nil) != c.wantErr {
				t.Fatalf("got %v %v", ok, err)
			}
		})
	}
}

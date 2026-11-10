package secret

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
}

func TestKeychainArgs(t *testing.T) {
	var calls []call
	k := Keychain{Run: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, call{name, args})
		return []byte("s3cret-pass\n"), nil
	}}
	pw, err := k.Get("me@x.example")
	if err != nil || pw != "s3cret-pass" {
		t.Fatalf("%q %v", pw, err)
	}
	if err := k.Set("me@x.example"); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"/usr/bin/security", []string{"find-generic-password", "-s", "mailkite", "-a", "me@x.example", "-w"}},
		{"/usr/bin/security", []string{"add-generic-password", "-U", "-s", "mailkite", "-a", "me@x.example", "-w"}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("got %v", calls)
	}
	for _, c := range calls {
		if strings.Contains(strings.Join(c.args, " "), "s3cret-pass") {
			t.Fatal("password in argv")
		}
	}
}

func TestKeychainNotFound(t *testing.T) {
	code := exec.Command("/bin/sh", "-c", "exit 44").Run()
	k := Keychain{Run: func(string, ...string) ([]byte, error) { return nil, code }}
	if _, err := k.Get("a@b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	other := errors.New("boom")
	k = Keychain{Run: func(string, ...string) ([]byte, error) { return nil, other }}
	if _, err := k.Get("a@b"); errors.Is(err, ErrNotFound) || err == nil {
		t.Fatalf("got %v", err)
	}
}

func TestKeychainKeepsSpaces(t *testing.T) {
	for in, want := range map[string]string{"  pa ss  \n": "  pa ss  ", "pw\r\n": "pw", "pw": "pw"} {
		k := Keychain{Run: func(string, ...string) ([]byte, error) { return []byte(in), nil }}
		got, err := k.Get("a@b")
		if err != nil || got != want {
			t.Fatalf("%q: got %q %v", in, got, err)
		}
	}
}

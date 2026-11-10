package safety

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateHideLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mailkite")
	st, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := st.Hide([]string{"a@x", "b@x", ""}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.Hide([]string{"a@x"}, now); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := st.Unhide([]string{"b@x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Log(ActionLine{TS: now, Action: "erase", Account: "UUID-1", Count: 3, Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Log(ActionLine{TS: now, Action: "erase", Account: "me@icloud.example", Count: 1, Result: "ok"}); err == nil {
		t.Fatal("address in log must be refused")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h, err := st.Hidden(context.Background())
	if err != nil || len(h) != 1 {
		t.Fatalf("%v %v", h, err)
	}
	if _, ok := h["a@x"]; !ok {
		t.Fatalf("%v", h)
	}
	if err := st.Log(ActionLine{TS: now.Add(time.Minute), Action: "unsubscribe", Account: "UUID-2", Count: 6, Result: "partial\nline"}); err != nil {
		t.Fatal(err)
	}

	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("dir mode %o", m)
	}
	for _, f := range []string{"state.sqlite", "actions.log"} {
		if m := mode(t, filepath.Join(dir, f)); m != 0o600 {
			t.Fatalf("%s mode %o", f, m)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "actions.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines %q", lines)
	}
	var l map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &l); err != nil {
		t.Fatal(err)
	}
	if len(l) != 5 || l["action"] != "erase" || l["account"] != "UUID-1" || l["count"] != 3.0 || l["result"] != "ok" || l["ts"] != now.Format(time.RFC3339) {
		t.Fatalf("line %v", l)
	}
}

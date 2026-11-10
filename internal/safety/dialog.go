package safety

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CancelButton is the dialog's default and cancel button.
const CancelButton = "Отмена"

// MaxDialogBody is the longest dialog body, in runes.
const MaxDialogBody = 1500

// Confirmer asks the user to confirm an action. It returns true only when the user
// pressed button; cancel, timeout and any failure mean "no".
type Confirmer interface {
	Confirm(ctx context.Context, title, body, button string) (bool, error)
}

const osascriptBin = "/usr/bin/osascript"

// dialogScript is fixed: user text never enters the AppleScript source, it arrives
// only as argv items (1 = title, 2 = body, 3 = button).
var dialogScript = []string{
	"-e", "on run argv",
	"-e", `display dialog (item 2 of argv) with title (item 1 of argv) buttons {"` + CancelButton + `", item 3 of argv} default button "` + CancelButton + `" cancel button "` + CancelButton + `" with icon caution giving up after 120`,
	"-e", "return button returned of result",
	"-e", "end run",
	"--",
}

// OSAScript shows a native dialog through /usr/bin/osascript. Run replaces process
// execution in tests; nil runs osascript and returns its stdout.
type OSAScript struct {
	Run func(ctx context.Context, args ...string) ([]byte, error)
}

func runOSAScript(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, osascriptBin, args...).Output()
}

// clean drops NUL and other control and bidi-override characters; with keepNL,
// line breaks survive (CRLF and CR become LF) and tabs become spaces.
func clean(s string, keepNL bool) string {
	s = strings.ToValidUTF8(s, "�")
	if keepNL {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && keepNL:
			return r
		case r == '\t' && keepNL:
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r), r == ' ', r == ' ':
			return -1
		}
		return r
	}, s)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// isUserCancel reports osascript's "User canceled. (-128)" exit.
func isUserCancel(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		return false
	}
	return bytes.Contains(ee.Stderr, []byte("User canceled")) || bytes.Contains(ee.Stderr, []byte("(-128)"))
}

// Confirm shows the dialog and returns true only if osascript printed exactly the
// action button. Cancel and the 120 s timeout return (false, nil); every other
// failure returns (false, err) — the action must then not run.
func (o OSAScript) Confirm(ctx context.Context, title, body, button string) (bool, error) {
	title = clean(title, false)
	body = truncateRunes(clean(body, true), MaxDialogBody)
	button = clean(button, false)
	if strings.TrimSpace(button) == "" || button == CancelButton {
		return false, fmt.Errorf("dialog: invalid action button %q", button)
	}
	run := o.Run
	if run == nil {
		run = runOSAScript
	}
	args := append(append([]string(nil), dialogScript...), title, body, button)
	out, err := run(ctx, args...)
	if err != nil {
		if isUserCancel(err) {
			return false, nil
		}
		return false, fmt.Errorf("dialog: osascript: %w", err)
	}
	return strings.TrimSuffix(string(out), "\n") == button, nil
}

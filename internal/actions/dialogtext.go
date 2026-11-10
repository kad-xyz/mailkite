package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fragRunes caps every variable fragment of dialog text (an address, a host, an
// account or mailbox name, a subject): sender-controlled text must never push the
// fixed parts of the dialog (counts, the protected warning) out of its body.
const fragRunes = 80

// maxMoveTo caps the move_to mailbox name, in runes.
const maxMoveTo = 100

// frag makes a variable fragment safe for the dialog: one line (line breaks and
// other spaces become one space; control, format and bidi characters are
// dropped) and at most fragRunes runes.
func frag(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Bidi_Control, r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > fragRunes {
		s = string([]rune(s)[:fragRunes-1]) + "…"
	}
	return s
}

// checkMoveToText accepts a single-line mailbox name of at most maxMoveTo runes.
func checkMoveToText(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("action move needs a target mailbox (move_to)")
	}
	if !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
	}) {
		return errors.New("move_to must be one line of plain text")
	}
	if utf8.RuneCountInString(name) > maxMoveTo {
		return fmt.Errorf("move_to is longer than %d characters", maxMoveTo)
	}
	return nil
}

// checkMoveTarget requires the mailbox name (exact, case-sensitive) in every given
// account of Mail's index, so a move never creates or guesses a mailbox.
func (e *Engine) checkMoveTarget(ctx context.Context, name string, accounts map[string]string) error {
	accs, err := e.Store.Accounts(ctx)
	if err != nil {
		return err
	}
	has := map[string]bool{}
	for _, a := range accs {
		for _, mb := range a.Mailboxes {
			if mb.Name == name {
				has[a.UUID] = true
			}
		}
	}
	for _, uuid := range sortedKeys(accounts) {
		if !has[uuid] {
			n := accounts[uuid]
			if n == "" {
				n = uuid
			}
			return fmt.Errorf("account %s has no mailbox %q (the name is case-sensitive, as find shows it)", n, name)
		}
	}
	return nil
}

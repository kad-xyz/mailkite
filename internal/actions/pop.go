package actions

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// POPDeleter permanently removes messages of a POP account from Mail. POP keeps no
// folders on the server, so the local copy in Mail is the message.
type POPDeleter interface {
	// Delete removes the messages with these Message-IDs from every mailbox of the
	// Mail account and then from its Deleted Messages. It returns the requested
	// Message-IDs (as given) that Mail found and deleted, each once.
	Delete(ctx context.Context, mailUUID string, messageIDs []string) (deleted []string, err error)
}

const osascriptBin = "/usr/bin/osascript"

// popScript is fixed: the account UUID and Message-IDs arrive only as argv items
// (1 = account UUID, 2.. = Message-IDs without angle brackets, as Mail reports them).
// It prints the Message-IDs it found and deleted, one per line.
var popScript = []string{
	"on run argv",
	"set accId to item 1 of argv",
	"set done to {}",
	"with timeout of 1800 seconds",
	`tell application "Mail"`,
	"set acc to account id accId",
	"repeat with i from 2 to count of argv",
	"set mid to item i of argv",
	"set got to false",
	"repeat with mb in (every mailbox of acc)",
	`if name of mb is not "Deleted Messages" then`,
	"set hits to (every message of mb whose message id is mid)",
	"repeat with m in hits",
	"delete m",
	"set got to true",
	"end repeat",
	"end if",
	"end repeat",
	"if got then set end of done to mid",
	"try",
	`delete (every message of mailbox "Deleted Messages" of acc whose message id is mid)`,
	"end try",
	"end repeat",
	"end tell",
	"end timeout",
	"set AppleScript's text item delimiters to linefeed",
	"set out to done as text",
	`set AppleScript's text item delimiters to ""`,
	"return out",
	"end run",
}

// POPScript returns the AppleScript source AppleScriptPOP runs (for checks).
func POPScript() string { return strings.Join(popScript, "\n") }

// AppleScriptPOP deletes POP messages through Mail's AppleScript in one osascript
// call. Run replaces process execution in tests; nil runs /usr/bin/osascript.
type AppleScriptPOP struct {
	Run func(ctx context.Context, args ...string) ([]byte, error)
}

// Delete implements POPDeleter.
func (a AppleScriptPOP) Delete(ctx context.Context, mailUUID string, messageIDs []string) ([]string, error) {
	if strings.TrimSpace(mailUUID) == "" {
		return nil, errors.New("pop delete: empty account")
	}
	orig := map[string]string{} // normalized -> as given (first one)
	var ids []string
	for _, id := range messageIDs {
		n := normID(id)
		if n == "" {
			continue
		}
		if _, dup := orig[n]; !dup {
			orig[n] = id
			ids = append(ids, n)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]string, 0, 2*len(popScript)+2+len(ids))
	for _, l := range popScript {
		args = append(args, "-e", l)
	}
	args = append(args, "--", mailUUID)
	args = append(args, ids...)
	run := a.Run
	if run == nil {
		run = func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, osascriptBin, args...).Output()
		}
	}
	out, err := run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("pop delete: osascript: %w", err)
	}
	// Only requested IDs count, each once, in request order.
	got := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		if n := normID(strings.TrimSuffix(l, "\r")); n != "" {
			got[n] = true
		}
	}
	var deleted []string
	for _, n := range ids {
		if got[n] {
			deleted = append(deleted, orig[n])
		}
	}
	return deleted, nil
}

// normID trims spaces and angle brackets from a Message-ID.
func normID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.TrimSpace(id)
}

package actions

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// POPDeleter permanently removes messages of a POP account from Mail. POP keeps no
// folders on the server, so the local copy in Mail is the message.
type POPDeleter interface {
	// Delete removes the messages with these Message-IDs from every mailbox of the
	// Mail account and then from its Deleted Messages, and returns how many it found.
	Delete(ctx context.Context, mailUUID string, messageIDs []string) (deleted int, err error)
}

const osascriptBin = "/usr/bin/osascript"

// popScript is fixed: the account UUID and Message-IDs arrive only as argv items
// (1 = account UUID, 2.. = Message-IDs without angle brackets, as Mail reports them).
var popScript = []string{
	"on run argv",
	"set accId to item 1 of argv",
	"set n to 0",
	"with timeout of 1800 seconds",
	`tell application "Mail"`,
	"set acc to account id accId",
	"repeat with i from 2 to count of argv",
	"set mid to item i of argv",
	"repeat with mb in (every mailbox of acc)",
	`if name of mb is not "Deleted Messages" then`,
	"set hits to (every message of mb whose message id is mid)",
	"repeat with m in hits",
	"delete m",
	"set n to n + 1",
	"end repeat",
	"end if",
	"end repeat",
	"try",
	`delete (every message of mailbox "Deleted Messages" of acc whose message id is mid)`,
	"end try",
	"end repeat",
	"end tell",
	"end timeout",
	"return n",
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
func (a AppleScriptPOP) Delete(ctx context.Context, mailUUID string, messageIDs []string) (int, error) {
	if strings.TrimSpace(mailUUID) == "" {
		return 0, errors.New("pop delete: empty account")
	}
	var ids []string
	for _, id := range messageIDs {
		if id = normID(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
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
		return 0, fmt.Errorf("pop delete: osascript: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("pop delete: unexpected osascript output %q", strings.TrimSpace(string(out)))
	}
	return min(n, len(ids)), nil
}

// normID trims spaces and angle brackets from a Message-ID.
func normID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.TrimSpace(id)
}

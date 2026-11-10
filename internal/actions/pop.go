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
	// Mail account and then from its trash. Deleted holds the requested
	// Message-IDs (as given) of which, after that, no copy is left anywhere in the
	// account; Left those Mail found but still has a copy of. IDs in neither were
	// not found.
	Delete(ctx context.Context, mailUUID string, messageIDs []string) (POPResult, error)
}

// POPResult is the verified outcome of POPDeleter.Delete.
type POPResult struct {
	Deleted, Left []string
}

const osascriptBin = "/usr/bin/osascript"

// popScript is fixed: the account UUID and Message-IDs arrive only as argv items
// (1 = account UUID, 2.. = Message-IDs without angle brackets, as Mail reports them).
// For each ID it deletes every copy in every mailbox of the account (nested ones
// too), then every copy in the account's trash ("Deleted Messages" and the
// account's mailbox of Mail's trash mailbox), and then counts the copies left in
// all of them. It prints "gone<TAB>id" only when Mail found the message and no
// copy is left, and "left<TAB>id" when one is; a failed count counts as a copy.
var popScript = []string{
	"on run argv",
	"set accId to item 1 of argv",
	"set out to {}",
	"with timeout of 1800 seconds",
	`tell application "Mail"`,
	"set acc to account id accId",
	"set trashes to {}",
	"try",
	`set end of trashes to mailbox "Deleted Messages" of acc`,
	"end try",
	"try",
	"repeat with tb in (every mailbox of trash mailbox)",
	"try",
	"if (id of account of tb) is accId then set end of trashes to contents of tb",
	"end try",
	"end repeat",
	"end try",
	"set boxes to my boxesOf(acc)",
	"repeat with i from 2 to count of argv",
	"set mid to item i of argv",
	"set got to false",
	"repeat with mb in (boxes & trashes)",
	"try",
	"set hits to (every message of mb whose message id is mid)",
	"if (count of hits) > 0 then set got to true",
	"repeat with m in hits",
	"delete m",
	"end repeat",
	"end try",
	"end repeat",
	"repeat with tb in trashes",
	"try",
	"set hits to (every message of tb whose message id is mid)",
	"if (count of hits) > 0 then set got to true",
	"repeat with m in hits",
	"delete m",
	"end repeat",
	"end try",
	"end repeat",
	"set remaining to 0",
	"repeat with mb in (boxes & trashes)",
	"try",
	"set remaining to remaining + (count of (every message of mb whose message id is mid))",
	"on error",
	"set remaining to remaining + 1",
	"end try",
	"end repeat",
	"if got and remaining is 0 then",
	`set end of out to "gone" & tab & mid`,
	"else if got then",
	`set end of out to "left" & tab & mid`,
	"end if",
	"end repeat",
	"end tell",
	"end timeout",
	"set AppleScript's text item delimiters to linefeed",
	"set res to out as text",
	`set AppleScript's text item delimiters to ""`,
	"return res",
	"end run",
	"on boxesOf(c)",
	"set res to {}",
	`tell application "Mail"`,
	"repeat with mb in (every mailbox of c)",
	"set end of res to contents of mb",
	"set res to res & (my boxesOf(contents of mb))",
	"end repeat",
	"end tell",
	"return res",
	"end boxesOf",
}

// POPScript returns the AppleScript source AppleScriptPOP runs (for checks).
func POPScript() string { return strings.Join(popScript, "\n") }

// AppleScriptPOP deletes POP messages through Mail's AppleScript in one osascript
// call. Run replaces process execution in tests; nil runs /usr/bin/osascript.
type AppleScriptPOP struct {
	Run func(ctx context.Context, args ...string) ([]byte, error)
}

// Delete implements POPDeleter. Only "gone" and "left" lines for requested IDs
// count, each ID once; "left" wins over "gone".
func (a AppleScriptPOP) Delete(ctx context.Context, mailUUID string, messageIDs []string) (POPResult, error) {
	if strings.TrimSpace(mailUUID) == "" {
		return POPResult{}, errors.New("pop delete: empty account")
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
		return POPResult{}, nil
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
		return POPResult{}, fmt.Errorf("pop delete: osascript: %w", err)
	}
	gone, left := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		status, id, ok := strings.Cut(strings.TrimSuffix(l, "\r"), "\t")
		if !ok {
			continue
		}
		switch n := normID(id); {
		case n == "":
		case status == "gone":
			gone[n] = true
		case status == "left":
			left[n] = true
		}
	}
	var res POPResult
	for _, n := range ids {
		switch {
		case left[n]:
			res.Left = append(res.Left, orig[n])
		case gone[n]:
			res.Deleted = append(res.Deleted, orig[n])
		}
	}
	return res, nil
}

// normID trims spaces and angle brackets from a Message-ID.
func normID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.TrimSpace(id)
}

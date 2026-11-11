<p align="center">
  <img src=".github/logo.svg" width="112" height="112" alt="mailkite logo">
</p>

<h1 align="center">mailkite</h1>

<p align="center">
  <b>A fast, token-lean MCP server for Apple Mail on macOS</b>
</p>

<p align="center">
  <a href="https://github.com/kad-xyz/mailkite/releases/latest"><img src="https://img.shields.io/github/v/release/kad-xyz/mailkite?color=6366F1" alt="Release"></a>
  <img src="https://img.shields.io/badge/macOS-Apple%20Mail-38BDF8?logo=apple&logoColor=white" alt="macOS">
  <img src="https://img.shields.io/github/go-mod/go-version/kad-xyz/mailkite?color=00ADD8&logo=go&logoColor=white" alt="Go version">
  <img src="https://img.shields.io/badge/MCP-stdio-111827" alt="MCP stdio">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/kad-xyz/mailkite?color=22C55E" alt="License: MIT"></a>
</p>

A small, fast MCP server for Apple Mail on macOS. It reads Mail's own local data directly, so searches take a fraction of a millisecond on a small mailbox (measured on about 110 messages), the process uses no CPU while idle, and the tool descriptions cost very few tokens.

It also cleans up: bulk erase, trash, move and mark, and unsubscribing from senders. Nothing is changed until you confirm in a native macOS dialog, and every erased message is archived locally first. Sending mail is on the roadmap.

## Why

- **Fast.** Reading never leaves the Mac: queries use the Envelope Index and `.emlx` files directly, with no AppleScript and no IMAP round trips.
- **No idle cost.** One static Go binary with no background work: no timers, no indexing daemons, so CPU is 0 while idle. (Memory is moderate, see the benchmark.)
- **Token-lean.** Nine tools, about 1,400 tokens of schema in total. Replies are one line per message; `read` returns at most 4000 characters by default and pages with `offset`.
- **Useful output.** `read` extracts verification codes, links and the attachment list up front, and gives a `message://` link that opens the message in Mail.
- **Safe cleanup.** Every change is previewed first and runs only after you press the button in a macOS dialog. The model cannot press it for you.

## Benchmark

Measured for the read-only stage (five tools) on one Mac (Apple silicon) with a stdio MCP client, same scenarios for each server, 7 calls per scenario, medians. The mailbox is small: about 110 messages across 3 accounts. Results on large mailboxes are not measured yet, so treat the numbers as a comparison of fixed overhead, not of scaling.

| | mailkite | rusty_apple_mail_mcp | mailidx | che-apple-mail-mcp |
|---|---|---|---|---|
| Tools | 5 | 4 | 26 | 59 |
| Schema size (est. tokens) | ~700 | ~3,600 | ~5,900 | ~24,000 |
| Startup | 5 ms | 17 ms | 29 ms | 95 ms |
| Accounts / status | 0.2 ms | 0.1 ms | 8.5 ms | 314 ms |
| Latest 50 messages | 0.3 ms | n/a (filter required) | 0.5 ms | 12 ms |
| Search by subject | 0.2 ms | 0.1 ms | 0.4 ms | 12.5 ms |
| Search by sender | 0.2 ms | 0.1 ms | 0.4 ms | 11.8 ms |
| Full-text search | 0.4 ms | n/a | 0.8 ms | 12.5 ms |
| Read one message | 0.3 ms (0.9 ms uncached) | 0.1 ms | 0.5 ms | 12.1 ms |
| Read reply size (chars) | 2,175 | 3,784 | 2,209 | 29,042 |
| Idle CPU (5 s) | 0 | 0 | 0 | 0.02 s |
| Peak RSS | 22.5 MB | 11.7 MB | 15.1 MB | 51.7 MB |
| Binary size (arm64) | 13 MB | 6.7 MB | 5.7 MB | 7.5 MB |

mailkite keeps the last parsed message, so the repeated read above is served from that cache; the uncached figure is the same message parsed from disk on every call. About 15 MB of its RSS is there before the first call: the MCP SDK links `net/http`, which on macOS loads the CoreFoundation and Security frameworks, and SQLite's code adds a few more once the mail index is opened.

mailkite is not the smallest or the lowest-memory option; its advantages here are the schema size and the compact, pre-digested replies. On this small mailbox every server except the AppleScript- and Swift-based one answers in about a millisecond or less, and mailidx's status call takes 8.5 ms.

## Install

Download a prebuilt binary (Apple silicon or Intel) from the [latest release](https://github.com/kad-xyz/mailkite/releases/latest):

```sh
curl -L -o mailkite https://github.com/kad-xyz/mailkite/releases/latest/download/mailkite-darwin-arm64   # or mailkite-darwin-amd64
chmod +x mailkite && xattr -d com.apple.quarantine mailkite 2>/dev/null; mv mailkite /usr/local/bin/
```

Or with a recent Go toolchain (see `go.mod`):

```sh
go install github.com/kad-xyz/mailkite/cmd/mailkite@latest
```

Or build from source:

```sh
git clone https://github.com/kad-xyz/mailkite
cd mailkite
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/mailkite ./cmd/mailkite
```

### Command line

Besides the stdio server (no arguments), the binary has these subcommands:

| Command | What it does |
|---|---|
| `mailkite setup` | Connect a Mail account for cleanup: choose it from the list, pick a preset (`icloud`, `gmail` or `manual`) and enter an app password. Run it in a terminal window. |
| `mailkite restore <path>` | Put archived messages back: one `.eml` file or a whole day directory of the archive. |
| `mailkite index` | Rebuild the full-text search index from scratch (otherwise it is built on the first `find` with `query`). |
| `mailkite version` | Print the version. |

### Full Disk Access

Mail's data lives in `~/Library/Mail`, which macOS protects. Grant Full Disk Access (System Settings, Privacy & Security, Full Disk Access) to the app that launches the server, not to the binary itself. For Claude Code that is your terminal app. After enabling it, fully quit and reopen that app.

### Connect to Claude Code

```sh
claude mcp add mailkite --scope user -- /path/to/mailkite
```

Any other MCP client that can launch a stdio server works the same way.

### Accounts for cleanup

Reading needs nothing beyond Full Disk Access. Changing mail on the server needs the account's password, so run `mailkite setup` once per account in a terminal window:

```sh
mailkite setup
```

It lists your Mail accounts with their addresses. Press Enter to keep the address Mail uses, choose a preset, then paste an **app password** when `security` asks for it twice (typing is not echoed):

- iCloud: [account.apple.com](https://account.apple.com) → Sign-In and Security → App-Specific Passwords.
- Gmail: [myaccount.google.com/apppasswords](https://myaccount.google.com/apppasswords) (needs 2-Step Verification).

The password goes straight into your login Keychain (service `mailkite`) and is never written to disk or shown to the model. `setup` logs in to check it before saving the account to `~/.config/mailkite/config.json` (mode `0600`). POP accounts are changed through Mail itself with AppleScript.

## Tools

| Tool | What it does |
|---|---|
| `status` | Accounts, message counts and search index state. |
| `find` | Search messages by text, sender, subject, date range, flags, mailbox or account; one line per message. |
| `read` | Read one message: headers, codes, links, attachments and text, with paging. |
| `save_attachment` | Save one or all attachments of a message to disk without overwriting existing files. |
| `senders` | Top senders with message counts, for finding newsletters and noise. |
| `cleanup` | Preview erasing, trashing, marking read or moving messages, by ids or filters, optionally unsubscribing too. |
| `unsubscribe` | Preview unsubscribing from senders. |
| `mark` | Mark messages read, unread, flagged or unflagged, or move them. |
| `execute` | Run a previewed plan after you confirm it in a macOS dialog. |

`status`, `find`, `read`, `save_attachment`, `senders`, `cleanup` and `unsubscribe` never change mail. `cleanup` and `unsubscribe` only return a plan.

## Writing safely

Every change follows the same path:

1. **Preview.** `cleanup`, `unsubscribe` or `mark` lists what would change, grouped by sender, and returns a `plan_id`. Nothing has been touched yet.
2. **Confirm.** `execute` with that `plan_id` opens a macOS dialog with the action, the count and the senders. **Cancel** is the default button; if nobody answers within 120 seconds, the plan is dropped. Only the button you press runs it. A plan works once and expires after 10 minutes.
3. **Act.** Before a message is erased, its full source is saved to the archive. Then it is removed on the server: IMAP accounts are changed directly (`UID EXPUNGE`, which needs the `UIDPLUS` extension); on Gmail the message is moved to Trash and erased there, so it also disappears from All Mail. Each message is checked by Message-ID before it is touched, so a stale index cannot hit the wrong one.

`mark` with read, unread, flag or unflag on up to 20 messages runs right away, since those changes are easy to undo. Moves and larger batches go through the dialog. The limit is `MarkLimit` in the config.

### Protected mail

Some mail is never included in a bulk action unless you name its id in `include_protected`:

- verification codes and password or sign-in messages;
- receipts, invoices, orders, tickets and bank statements;
- mail from banks and government services;
- mail from people (not lists or automated senders);
- anything matching your own rules in the config: `ProtectedDomains`, `ProtectedAddresses` and `ProtectedKeywords`.

Previews say how many messages were left out and why.

### Unsubscribing

mailkite uses only the sender's `List-Unsubscribe` header, never links from the body:

- **one-click** (RFC 8058): an HTTPS POST; private, local and other internal addresses are refused;
- **mail**: a message with the fixed text "unsubscribe" to the one address in the header, sent from the account that received the newsletter (never from another of your addresses);
- otherwise the preview shows the link to open yourself.

### Archive and restore

Erased messages are kept as `.eml` files in `~/Library/Application Support/mailkite/archive/<day>/` for 30 days. To put them back:

```sh
mailkite restore ~/Library/Application\ Support/mailkite/archive/2026-11-11
```

Actions are logged to `~/Library/Application Support/mailkite/actions.log`: time, action, account, count and result. Subjects and addresses are not logged.

## Notes

- The first `find` with `query` builds the full-text search index inside that call; later calls only add new messages.
- A `query` combined with narrowing filters (account, mailbox, sender, dates, flags) searches the top 500 text matches and then applies the filters.
- `save_attachment` writes only inside `~/Downloads` (the default; subfolders are allowed) and never overwrites existing files. To allow other folders, set `MAILKITE_SAVE_DIRS` in the server's environment to a colon-separated list of absolute paths; `~/Library` and dot-folders are always refused, and the folders cannot be chosen through tool arguments.
- `read` returns up to 4000 characters by default (maximum 20000); continue with `offset`.

## Privacy

Reading is local: Mail's data is opened read-only and no network connection is made. The search index is stored at `~/Library/Caches/mailkite` with `0600` permissions and can be deleted at any time; it is rebuilt on demand.

mailkite connects to the network only when you confirm an action: to your mail servers (IMAP, and SMTP for unsubscribe-by-mail) and to the one-click unsubscribe URL of a sender you chose.

It is reasonable to auto-approve the tools that never change mail (`status`, `find`, `read`, `senders`, `cleanup`, `unsubscribe`) and `save_attachment`, which is confined to `~/Downloads` plus any folders you list in `MAILKITE_SAVE_DIRS` (list only folders you would not mind receiving files). Leave `mark` and `execute` on manual approval. The dialog still protects you if you don't, but there is no reason to remove a layer.

Note that mailkite reads the password through the `security` command-line tool, which the Keychain item trusts. Any process running as your user can read it the same way, as with any password stored by `security`. Use an app password, not your account password, so you can revoke it at any time.

## Roadmap

Writing and sending mail, gated by the same confirmation dialog.

## License

MIT

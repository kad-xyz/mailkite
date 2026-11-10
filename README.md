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

Stage 1 is read-only. Writing tools (move, delete, send) are on the roadmap and will require a native macOS confirmation dialog.

## Why

- **Fast.** Queries read the Envelope Index and `.emlx` files directly: no AppleScript, no IMAP round trips.
- **No idle cost.** One static Go binary with no background work: no timers, no indexing daemons, so CPU is 0 while idle. (Memory is moderate, see the benchmark.)
- **Token-lean.** Five tools, about 700 tokens of schema in total. Replies are one line per message; `read` returns at most 4000 characters by default and pages with `offset`.
- **Useful output.** `read` extracts verification codes, links and the attachment list up front, and gives a `message://` link that opens the message in Mail.

## Benchmark

Measured on one Mac (Apple silicon) with a stdio MCP client, same scenarios for each server, 7 calls per scenario, medians. The mailbox is small: about 110 messages across 3 accounts. Results on large mailboxes are not measured yet, so treat the numbers as a comparison of fixed overhead, not of scaling.

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

Besides the stdio server (no arguments), the binary has two subcommands: `mailkite index` builds the full-text search index from scratch (otherwise it is built on the first `find` with `query`), and `mailkite version` prints the version.

### Full Disk Access

Mail's data lives in `~/Library/Mail`, which macOS protects. Grant Full Disk Access (System Settings, Privacy & Security, Full Disk Access) to the app that launches the server, not to the binary itself. For Claude Code that is your terminal app. After enabling it, fully quit and reopen that app.

### Connect to Claude Code

```sh
claude mcp add mailkite --scope user -- /path/to/mailkite
```

Any other MCP client that can launch a stdio server works the same way.

## Tools

| Tool | What it does |
|---|---|
| `status` | Accounts, message counts and search index state. |
| `find` | Search messages by text, sender, subject, date range, flags, mailbox or account; one line per message. |
| `read` | Read one message: headers, codes, links, attachments and text, with paging. |
| `save_attachment` | Save one or all attachments of a message to disk without overwriting existing files. |
| `senders` | Top senders with message counts, for finding newsletters and noise. |

All five tools are annotated read-only.

## Notes

- The first `find` with `query` builds the full-text search index inside that call; later calls only add new messages.
- A `query` combined with narrowing filters (account, mailbox, sender, dates, flags) searches the top 500 text matches and then applies the filters.
- `save_attachment` writes only inside `~/Downloads` (the default; subfolders are allowed) and never overwrites existing files. To allow other folders, set `MAILKITE_SAVE_DIRS` in the server's environment to a colon-separated list of absolute paths; `~/Library` and dot-folders are always refused, and the folders cannot be chosen through tool arguments.
- `read` returns up to 4000 characters by default (maximum 20000); continue with `offset`.

## Privacy

Everything is local. mailkite makes no network connections. Mail's data is opened read-only. The search index is stored at `~/Library/Caches/mailkite` with `0600` permissions and can be deleted at any time; it is rebuilt on demand.

Since all current tools are read-only and `save_attachment` is confined to `~/Downloads` (plus any folders you list in `MAILKITE_SAVE_DIRS`, so list only folders you would not mind receiving files), it is reasonable to auto-approve them in your MCP client. Do not blanket-approve future writing tools.

## Roadmap

Writing tools (move, archive, delete, send) are planned, each gated by a macOS confirmation dialog.

## License

MIT

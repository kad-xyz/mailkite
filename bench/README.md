# Benchmark

The scripts and raw results behind the [benchmark table](../README.md#benchmark) in the main README.

## What is here

| Path | What it is |
|---|---|
| `mcpbench.py` | A minimal stdio MCP client. It starts a server, measures startup, tool count and schema size, times each call, then samples peak RSS and idle CPU with `ps`. Python 3 only, no dependencies. |
| `schemas.py` | Prints the input schema of chosen tools, to find the right arguments for a server. |
| `specs/*.json` | The scenarios for each server: the same six requests mapped to each server's own tools. |
| `results/*.json` | The raw output for each server from the run on 2026-11-10 (read-only stage of mailkite, five tools). |

Servers compared:

- [che-apple-mail-mcp](https://github.com/PsychQuant/che-apple-mail-mcp) (Swift)
- [rusty_apple_mail_mcp](https://github.com/like-a-freedom/rusty_apple_mail_mcp) (Rust)
- [mailidx](https://github.com/jx-grxf/mailidx) (Rust)
- mailkite (Go)

## Run it

The terminal needs Full Disk Access, as for mailkite itself.

1. Download each server's release binary and check its SHA-256.
2. Edit the specs: replace `SUBJECT_WORD`, `sender@example.com`, `BODY_WORD`, `ACCOUNT_NAME` and the message id `12345` with values that exist in your own mailbox. The specs in the repo are placeholders; the results were measured with real values.
3. Run each server:

```sh
python3 bench/mcpbench.py list -- /path/to/mailkite                      # tools and schema size only
python3 bench/mcpbench.py call bench/specs/mailkite.json -- /path/to/mailkite > mailkite.json
```

Each scenario is called 7 times and the median is reported. After the calls the client waits 5 seconds and records the CPU time spent while idle.

To look up a server's arguments:

```sh
python3 bench/schemas.py bench find,read -- /path/to/mailkite
```

## Reading the results

| Field | Meaning |
|---|---|
| `startup_s` | From process start to the `initialize` reply. |
| `tools`, `schema_chars`, `schema_tokens_est` | Size of `tools/list`; tokens are estimated as characters / 4. |
| `calls[].median_ms`, `min_ms` | Time per call. |
| `calls[].resp_chars` | Size of the reply the model would receive. |
| `calls[].error` | Error text if the call failed. rusty_apple_mail_mcp refuses a search without a filter, so its `recent50` is reported as n/a. |
| `idle_cpu_5s` | CPU seconds used over 5 seconds of doing nothing. |
| `peak_rss_mb` | Highest resident memory seen during the run. |

The mailbox was small (about 110 messages across 3 accounts), so these numbers compare fixed overhead, not scaling.

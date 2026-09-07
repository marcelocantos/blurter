# blurter

blurter is a local notification daemon: applications spool events to disk, and a
single daemon owns the Slack credential, the delivery policy, and the sinks.

Domain guide (severities, dedup/recovery policy, config reference, gotchas):
[`agents-guide.md`](agents-guide.md), also served by `blurter --help-agent`.
User-facing overview: [`README.md`](README.md). This file covers only what an
agent needs to *work on* the repo; do not duplicate the other two here.

## Build, test, run

```sh
make            # vet + test + build (the default target)
make build      # go build -ldflags ... -o blurter .
make test       # go test ./... -count=1
make vet        # go vet ./...
make lint       # fails if gofmt -l . is non-empty; CI enforces this
make fmt        # gofmt -w .
```

Never pass `-j` to make (the Makefile says so; set `MAKEFLAGS` there if needed).
`make install` copies the binary to `~/.local/bin` — do not run it in a burst or
worktree, it replaces a live binary.

CI (`.github/workflows/ci.yml`) runs gofmt, `go vet`, `go test -race`, `go
build`, and a CLI contract check that `--version`, `--help` and `--help-agent`
all work. Keep that contract.

## Layout

Single Go module, one package `main`, flat at the repo root.

| File | Role |
|------|------|
| `main.go` | CLI dispatch, `usage` text, `send`/`daemon`/`drain`/`status` commands |
| `config.go` | `Paths` (XDG resolution), `Config`, per-app policy merge, Slack token loading with the mode-600 refusal |
| `event.go` | `Event`, `Severity`, dedup digest, atomic spool write |
| `policy.go` | `Decide` — pure policy: mute, min-severity, silent `ok`, `RECOVERED`, `STILL BROKEN` |
| `daemon.go` | `Drainer`: spool scan, retry/backoff, parking, audit log, destination resolution |
| `sinks.go` | `SlackSink` (chat.postMessage), `BannerSink` (terminal-notifier, osascript fallback) |
| `service.go` | launchd plist generation, `service install`/`uninstall`/`path` |
| `mcp.go` | JSON-RPC-over-stdio MCP server: `blurter_send`, `blurter_status`, `blurter_recent` |
| `docs/audits/` | entropy/hygiene audit reports |
| `bullseye.yaml` | convergence targets (edit via the bullseye MCP tools, never by hand) |

Tests are `*_test.go` beside their subject: `drain_test.go`, `policy_test.go`,
`mcp_test.go`.

## Invariants worth not breaking

- **Submitting never delivers.** `send` and `blurter_send` write a spool file
  and return. Delivery lives in the daemon; no LLM anywhere in that path.
- **Delivery state is recorded only after a sink confirms**, so an outage can
  never mark an undelivered alert as sent.
- **Undeliverable events are parked, never deleted** (`$XDG_STATE_HOME/blurter/parked/`).
- **`Decide` stays pure** — no state mutation, no I/O; that is what makes the
  policy testable.
- **Config resolves from `$XDG_CONFIG_HOME`, never from the executable's
  prefix.** A package-managed prefix is replaced wholesale on upgrade.
- **Transient failures retry, permanent ones do not** (`invalid_auth`,
  `missing_scope`).
- **Do not send real Slack messages from tests or agent sessions.** Sinks are
  faked in tests; `blurter drain` against a configured token delivers for real.

## Using blurter as an MCP server

```json
{
  "mcpServers": {
    "blurter": { "command": "blurter", "args": ["mcp"] }
  }
}
```

Or `claude mcp add --scope user blurter -- blurter mcp`. Three tools only —
parameters are listed in `agents-guide.md`, and the registrations live in
`mcpTools()` in `mcp.go`; change both together.

## Naming

There is one binary, `blurter`. `blurterd` is not a program: it is the launchd
label `com.marcelocantos.blurterd` and the daemon's log filename. The daemon is
`blurter daemon`.

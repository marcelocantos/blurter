# STABILITY

Blurter is pre-1.0 (`v0.1.0`). This is the interaction surface as shipped,
not the intended contract. Where README examples and HEAD disagree, HEAD wins.

## Stability commitment

Until 1.0, CLI flags, MCP schemas, config keys, and policy may change between
minor releases. After 1.0, breaking those would require a major bump. The
pre-1.0 period exists to get them right.

## Policy (shipped)

`Decide` (`policy.go`) is the engine. Order is mute, then `min_severity`, then
recovery / dedup / deliver.

**Severity rank** (`event.go`): `info` = 0, `ok` = 1, `problem` = 2.

**`min_severity`** is a numeric floor applied *before* recovery. An event with
`rank < min` is suppressed as "below configured minimum". Default (empty in
config) is `info`, which drops nothing.

Consequences that match HEAD:

- `min_severity: problem` delivers problems only. `ok` ranks below `problem`,
  so that app **never** sends `RECOVERED`. The README `chatty-app` example is
  this config.
- `min_severity: ok` drops `info` only; `ok` can still recover.
- Recovery (`RECOVERED — ` + subject) fires only when severity is `ok`, the
  previous remembered status is `problem`, *and* `ok` passed the floor.

**Mute** suppresses everything, including problems.

**Dedup** hashes subject + body unless `--key` overrides. An identical problem
is held until `renotify_after` (default 168h), then prefixed
`STILL BROKEN (Nd)`. Zero `renotify_after` disables re-notification.

**Suppressed events still update last status.** `RecordSuppressed` overwrites
`LastStatus` and does not touch `LastSentAt`. A dropped `info` after a problem
clears the outstanding-problem bit, so a later `ok` has nothing to close out.

**State is recorded only after a sink confirms.** A delivery failure cannot
mark an alert sent.

**Any sink succeeding counts as delivered.** Banner defaults on, so Slack can
fail permanently while the event is still removed from the spool.

## Retry and parking

Backoff is `30s × 2^n`, capped at 15m, from the event's **`CreatedAt`**, not
the last attempt (`daemon.go`). `maxAttempts` is 12. The in-memory attempt
map is dropped on daemon restart (retry immediately). After the cap, or on
permanent sink failure, the file moves to `$XDG_STATE_HOME/blurter/parked/`.
Parked files are not retried.

Unreadable spool `*.json` is logged every poll and left in place; it is not
parked.

## CLI

| Command | What it does |
|---|---|
| `blurter send` | Validate + atomic spool. No config, no token, no network. Requires `--app`, `--severity`, `--subject`. |
| `blurter daemon [--interval]` | Drain loop (default 5s). Alias: `serve`. |
| `blurter drain` | One drain, then exit. |
| `blurter status` | Config/state/spool **paths**, spool pending count, spool errors, slack/banner, per-app state. **Does not report `parked/`.** |
| `blurter service install\|uninstall\|path` | launchd (macOS) only. `--dry-run` prints the plist. Resolves `os.Executable()` through `EvalSymlinks`. Log: `~/.local/var/log/blurter/blurterd.log`. |
| `blurter mcp` | MCP stdio. Tools spool and inspect; they do not deliver. |
| `--version` / `--help` / `--help-agent` | CLI contract. `--help-agent` is usage plus embedded `agents-guide.md`. |

There is no `--json` flag.

Homebrew installs are meant to use `brew services` (`opt_bin/"blurter" daemon`,
log under Homebrew `var/log`). `blurter service install` is the from-source
path and is not equivalent.

## MCP

Tools: `blurter_send` (defaults `app=adhoc`, `severity=info`; requires
`subject`), `blurter_status` (spool, sinks, **parked count when non-empty**,
apps; no paths), `blurter_recent` (audit). Same spool as the CLI.

## Config and paths

- Config: `$XDG_CONFIG_HOME/blurter/config.yaml` (never next to the binary).
- Token file: mode `0600` or stricter, or blurter refuses to read it.
- State / spool / audit: `$XDG_STATE_HOME/blurter/`.
- Missing config is not fatal: banner-only fallback.
- Corrupt `state.json` is not fatal: empty state, risk of one duplicate.

YAML keys: `slack.token_file`, `slack.dm`, `slack.channel`, `banner.enabled`
(default true), `default` / `apps.<name>` with `destination`,
`renotify_after`, `min_severity`, `mute`.

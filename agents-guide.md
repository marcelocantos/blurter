# blurter — agent guide

blurter is a local notification daemon. Applications **spool** events; blurter
owns the Slack credential, the delivery policy, and the sinks.

## The one thing to understand

`blurter send` does **not** deliver. It writes a JSON event into a spool
directory and exits, typically in a millisecond. A separate daemon (`blurterd`)
drains the spool and delivers with retry.

This asynchrony is the entire design, for three reasons:

1. **Submitting apps hold no credential.** Rotating the Slack token touches one
   file, not every app that notifies.
2. **Submitting apps never block on the network.** A notification cannot slow
   down or fail the work that raised it.
3. **Alerts survive being offline.** A scheduled job that fires while the
   machine has no Wi-Fi still gets its alert delivered — later, but delivered.
   Synchronous notifiers drop it, and if they also record it as "sent", dedup
   suppresses the retry and the alert is lost permanently. That exact bug is why
   blurter exists.

So: `blurter send` exiting 0 means **spooled**, not **delivered**. Do not treat
it as delivery confirmation, and do not fail your app when it errors.

## Sending an event

```sh
blurter send --app myapp --severity problem \
  --subject "nightly backup failed" \
  --body "3 of 40 volumes did not complete; see the log" \
  --link ~/.local/var/log/myapp.log
```

Body from stdin, which is usually what a script wants:

```sh
printf '%s\n' "${issues[@]}" | blurter send --app myapp --severity problem \
  --subject "myapp unhealthy (${#issues[@]} issues)" --body -
```

### Severities

| Severity  | Meaning | Delivery |
|-----------|---------|----------|
| `problem` | Something is wrong; interrupt the human. | Delivered, subject to dedup. |
| `ok`      | Healthy. | **Silent**, unless it closes out a previously reported problem *and* `ok` meets `min_severity` — then one `RECOVERED` notice. |
| `info`    | Notable non-failure. | Delivered only if the app's `min_severity` allows it. |

Rank is `info` < `ok` < `problem`. `min_severity` is a floor applied **before**
the recovery branch. `min_severity: problem` therefore drops `ok` as "below
configured minimum" and that app never sends `RECOVERED`.

Report `ok` on every healthy run when `min_severity` is `info` (the default). It
is silent, and it is what lets blurter tell you when something recovers. An app
that only reports failures can never produce a recovery notice. An app whose
config is `min_severity: problem` cannot produce one either.

### `--key`

Dedup normally hashes subject + body, so a *changed* problem alerts again. When
the text varies but the underlying problem does not — an embedded retry count,
a timestamp, a fluctuating item count — pass a stable `--key` so repeats are
recognised as the same problem:

```sh
blurter send --app myapp --severity problem --key backup-volumes-failing \
  --subject "backup failed: 3 volumes" --body -
```

## Policy, and what it means for callers

blurter decides whether to actually notify. Callers do not need to implement any
of this, and should not try:

- **Dedup.** An identical problem is not re-sent until `renotify_after`
  (default 7 days), then arrives prefixed `STILL BROKEN (Nd)`.
- **Recovery.** The first `ok` after a delivered `problem` sends one
  `RECOVERED` notice — only if `ok` is at or above `min_severity`. Default
  `min_severity: info` lets this fire; `min_severity: problem` does not.
- **Steady-state silence.** Repeated `ok` reports are silent.
- **Per-app isolation.** State is per app, so one app's alerts never suppress
  another's, and one app's recovery never closes another's problem.
- **Suppressed events still change last status.** A dropped `info` (or `ok`)
  overwrites the app's remembered severity without touching the re-notify
  timestamp. After a problem, that can clear the outstanding-problem bit so a
  later `ok` has nothing to recover.

Every decision — including every suppression, with its reason — is recorded in
`$XDG_STATE_HOME/blurter/audit.jsonl`, so "blurter ate my notification" is
always answerable.

## Configuration

`$XDG_CONFIG_HOME/blurter/config.yaml` (normally `~/.config/blurter/`):

```yaml
slack:
  token_file: slack-token     # relative to the config dir; mode must be 600
  dm: U01234567               # your Slack member ID
  # channel: C01234567

banner:
  enabled: true               # desktop notification; no credential, works offline

default:
  renotify_after: 168h
  min_severity: info

apps:
  myapp:
    renotify_after: 24h       # nag sooner about this one
  chatty-app:
    min_severity: problem     # problems only: also drops ok, so no RECOVERED
  under-development:
    mute: true                # silence entirely without touching the app
```

Config lives in `$XDG_CONFIG_HOME`, **never** beside the installed binary. A
package-managed prefix (`/opt/homebrew/Cellar/...`) is replaced wholesale on
every upgrade, so config resolved relative to the executable silently
disappears. Resolving config that way cost a sibling project fifteen days of
silent no-ops.

The token file is refused unless its mode is `600` or stricter. That is a loud
failure on purpose.

## Using blurter as an MCP server

If blurter is registered as an MCP server (`claude mcp add --scope user blurter
-- blurter mcp`), prefer the tools over shelling out:

- **`blurter_send`** — raise a notification. Same asynchrony as the CLI: the
  reply means spooled, not delivered.
- **`blurter_status`** — spool depth, sink readiness, per-app status, and
  parked count when `parked/` is non-empty. CLI `blurter status` reports paths
  and spool errors but does **not** mention `parked/`.
- **`blurter_recent`** — audit entries, to answer "did that actually go out?"

`blurter_send` defaults `app` to `adhoc` and `severity` to `info`, which is
right for "tell me when this finishes". Use `problem` only for something
actually wrong, or you erode the meaning of the severity that interrupts a
human.

## Running the daemon

```sh
blurter service install     # generates and loads a launchd agent (from-source)
blurter status              # paths, spool depth, sink readiness, per-app state
blurter drain               # deliver what's spooled now, then exit
```

On Linux, run `blurter daemon` under `systemd --user`; `service install` is
launchd-only. Homebrew users should use `brew services`, not `service install`:
the generated plist is not the formula's (different log path, PATH, and it
records `EvalSymlinks` of the binary, which on Homebrew is a Cellar path).

`blurter drain` is useful when there is no daemon, and in tests. It is not a
substitute for the daemon: without one, spooled events wait until something
calls `drain`.

## Gotchas

- **`send` exiting 0 is not delivery.** See above. Confirm in the audit log or
  with `blurter_recent`. `blurter status` is sink/spool health, not a per-event
  receipt.
- **A missing config file is not an error.** blurter falls back to the banner
  sink, because dropping every event would be worse than notifying locally.
  Check `blurter status` to confirm Slack is actually wired up.
- **`ok` events are silent by design.** If you are testing delivery, send a
  `problem`.
- **Undeliverable events are parked, not deleted** (in
  `$XDG_STATE_HOME/blurter/parked/`) so blurter's own failure never destroys an
  alert. A growing `parked/` means a permanent misconfiguration. Look in that
  directory, the audit log (`parked:` lines), or MCP `blurter_status`. CLI
  `blurter status` does not report parked count.
- **Never put an LLM in the delivery path.** blurter is deliberately a plain
  HTTPS call. Measured on 2026-07-26, a headless `claude` had its Slack
  connector available in 1 of 6 invocations, which disqualifies it for anything
  that must work while things are broken. An agent may usefully *read* the audit
  log or *submit* events; it must not be what delivers them.

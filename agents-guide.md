# blurter — agent guide

blurter is a local notification daemon. Applications **spool** events; blurter
owns the Slack credential, the delivery policy, and the sinks.

## The one thing to understand

`blurter send` does **not** deliver. It writes a JSON event into a spool
directory and exits, typically in a millisecond. A separate process — `blurter
daemon`, the same binary — drains the spool and delivers with retry. (There is
no `blurterd` binary; that name survives only as the launchd label
`com.marcelocantos.blurterd` and its log filename.)

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

`send` prints the spooled event's path; `--quiet` suppresses that.

Body from stdin, which is usually what a script wants:

```sh
printf '%s\n' "${issues[@]}" | blurter send --app myapp --severity problem \
  --subject "myapp unhealthy (${#issues[@]} issues)" --body -
```

### Severities

| Severity  | Meaning | Delivery |
|-----------|---------|----------|
| `problem` | Something is wrong; interrupt the human. | Delivered, subject to dedup. |
| `ok`      | Healthy. | **Silent**, unless it closes out a problem blurter previously reported — then one `RECOVERED` notice. |
| `info`    | Notable non-failure. | Delivered only if the app's `min_severity` allows it. |

Report `ok` on every healthy run. It costs nothing (it's silent) and it is what
lets blurter tell you when something recovers. An app that only reports failures
can never produce a recovery notice.

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
- **Recovery.** The first `ok` after a `problem` sends one `RECOVERED` notice.
- **Steady-state silence.** Repeated `ok` reports are silent.
- **Per-app isolation.** State is per app, so one app's alerts never suppress
  another's, and one app's recovery never closes another's problem.

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
    min_severity: problem     # drop its info events
  under-development:
    mute: true                # silence entirely without touching the app
  noisy-batch:
    destination: channel      # dm (default) | channel | an explicit ID
```

Any `apps.<name>` key is also valid under `default`. `destination` resolves
three ways: `dm` or unset uses `slack.dm`, `channel` uses `slack.channel`, and
anything else is taken literally as a channel or member ID.
`renotify_after: 0s` suppresses an unchanged problem indefinitely.

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
  reply means spooled, not delivered. Parameters: `subject` (required), `body`,
  `severity` (`problem`|`ok`|`info`), `app`, `key`, `link`.
- **`blurter_status`** — spool depth, sink readiness, per-app status. No
  parameters.
- **`blurter_recent`** — audit entries, to answer "did that actually go out?".
  Parameters: `limit` (default 20), `app`.

That is the whole surface: three tools, no others.

`blurter_send` defaults `app` to `adhoc` and `severity` to `info`, which is
right for "tell me when this finishes". Use `problem` only for something
actually wrong, or you erode the meaning of the severity that interrupts a
human.

## Running the daemon

```sh
blurter service install     # generates and loads a launchd agent
blurter service install --dry-run   # print the plist instead of writing it
blurter service path        # where the plist lives
blurter service uninstall   # unload and remove it
blurter daemon --interval 5s        # run in the foreground (default 5s)
blurter status              # spool depth, per-app state, sink readiness
blurter drain               # deliver what's spooled now, then exit
```

The banner sink shells out to `terminal-notifier`, falling back to `osascript`;
with neither present the banner sink fails and only Slack delivers.

On Linux, run `blurter daemon` under `systemd --user`; `service install` is
launchd-only.

`blurter drain` is useful when there is no daemon, and in tests. It is not a
substitute for the daemon: without one, spooled events wait until something
calls `drain`.

## Gotchas

- **`send` exiting 0 is not delivery.** See above. Check `blurter status` or the
  audit log for delivery.
- **A missing config file is not an error.** blurter falls back to the banner
  sink, because dropping every event would be worse than notifying locally.
  Check `blurter status` to confirm Slack is actually wired up.
- **`ok` events are silent by design.** If you are testing delivery, send a
  `problem`.
- **Undeliverable events are parked, not deleted** (in
  `$XDG_STATE_HOME/blurter/parked/`) so blurter's own failure never destroys an
  alert. An event is parked on the first permanent failure (`invalid_auth`,
  `missing_scope`) or after 12 transient attempts, so a growing `parked/` most
  often means a misconfiguration — check `blurter status` and the audit log.
- **Never put an LLM in the delivery path.** blurter is deliberately a plain
  HTTPS call. Measured on 2026-07-26, a headless `claude` had its Slack
  connector available in 1 of 6 invocations, which disqualifies it for anything
  that must work while things are broken. An agent may usefully *read* the audit
  log or *submit* events; it must not be what delivers them.

# blurter

A local notification daemon. Applications **spool** events; blurter owns the
credentials, the delivery policy, and the sinks.

```
  your app ──► blurter send ──► spool/<id>.json      (instant, offline-safe)
                                      │
                          blurterd ───┴──► policy ──► Slack DM / channel
                                              └─────► desktop banner
                                              └─────► retry with backoff
```

## Why

Every scheduled job eventually grows a notifier, and every one of them gets the
same things wrong. blurter exists so that happens once, centrally:

- **Apps hold no credentials.** Rotating a Slack token touches one file, not
  every app that notifies.
- **Apps never block on the network.** `blurter send` writes a file and exits.
  A notification cannot slow down, or fail, the work that raised it.
- **Alerts survive being offline.** This is the one that matters. A scheduled job
  firing while the machine has no network still gets its alert — delivered late,
  but delivered. A synchronous notifier drops it; and if it also records the
  alert as "sent", its own dedup then suppresses the retry and the alert is lost
  permanently. blurter was written after exactly that bug cost a sibling project
  a fortnight of silent failures.
- **One policy, implemented once.** Dedup, re-notify windows, recovery notices,
  per-app severity thresholds and muting.

## Install

```sh
brew install marcelocantos/tap/blurter
./supervisor/install.sh
```

On macOS, the installer registers blurter with Supervisor and stops any older
Homebrew or blurter launchd job. Supervisor runs the installed Homebrew binary,
restarts it if it exits, and writes `~/.local/var/log/blurter.log`. Check it with
`supervisorctl status blurter`. Install Supervisor first if it is not already
managing your other services. On Linux, run `blurter daemon` under
`systemd --user`.

From source: `go build -o blurter .`

## Configure

`~/.config/blurter/config.yaml`:

```yaml
slack:
  token_file: slack-token     # relative to this dir; mode must be 600
  dm: U01234567               # your Slack member ID

banner:
  enabled: true               # desktop notification; no credential, works offline

default:
  renotify_after: 168h        # an unchanged problem re-alerts weekly
  min_severity: info

apps:
  ytt:
    renotify_after: 24h
  chatty-app:
    min_severity: problem
  under-development:
    mute: true
```

For Slack, create an app with the `chat:write` and `im:write` bot scopes, install
it to your workspace, and store the `xoxb-…` token:

```sh
mkdir -p ~/.config/blurter
printf '%s\n' 'xoxb-…' > ~/.config/blurter/slack-token
chmod 600 ~/.config/blurter/slack-token
```

blurter refuses to read the token unless its mode is `600` or stricter — a loud
failure beats a quiet credential leak.

Config lives in `$XDG_CONFIG_HOME`, never beside the installed binary. A
package-managed prefix is replaced wholesale on upgrade, so config resolved
relative to the executable silently disappears.

## Use

```sh
blurter send --app myapp --severity problem \
  --subject "nightly backup failed" \
  --body "3 of 40 volumes did not complete" \
  --link ~/.local/var/log/myapp.log
```

Body from stdin, which is usually what a script wants:

```sh
printf '%s\n' "${issues[@]}" | blurter send --app myapp --severity problem \
  --subject "myapp unhealthy (${#issues[@]} issues)" --body -
```

| Severity  | Meaning | Delivery |
|-----------|---------|----------|
| `problem` | Something is wrong. | Delivered, subject to dedup. |
| `ok`      | Healthy. | **Silent**, unless closing out a reported problem — then one `RECOVERED` notice. |
| `info`    | Notable non-failure. | Delivered if `min_severity` allows. |

Report `ok` on every healthy run: it is silent, and it is what lets blurter tell
you when something recovers.

> `blurter send` exiting 0 means **spooled**, not **delivered**. Delivery is
> asynchronous by design. Use `blurter status` or the audit log to confirm.

## Ad-hoc messages from an agent session

blurter is an MCP server, so an agent can raise a notification directly —
useful for "tell me when this finishes" on a long task:

```sh
claude mcp add --scope user blurter -- blurter mcp
```

Tools: `blurter_send` (spool an event), `blurter_status` (health), and
`blurter_recent` (what was delivered or suppressed, and why).

Note what the MCP surface deliberately does **not** do: deliver. The tools spool
and read, exactly like the CLI. Delivery stays in the daemon, with no LLM in
that path — an agent asking for a message is a submitter like any other app, and
gets the same durability and policy.

## Inspect

```sh
blurter status     # spool depth, per-app state, sink readiness
blurter drain      # deliver what's spooled now, then exit
```

Every decision — including each suppression and its reason — is appended to
`~/.local/state/blurter/audit.jsonl`, so "blurter ate my notification" is always
answerable. Undeliverable events are moved to `parked/` rather than deleted:
blurter's own failure must never destroy an alert.

## Design notes

**Spool-first, at-least-once.** The spool is the contract. Submitting is a
durable atomic file write (temp + `fsync` + rename), so a reader never sees a
partial event and a power cut cannot lose one. Delivery state is recorded *only*
after a sink confirms, so an outage can never mark an undelivered alert as sent.

**No LLM in the delivery path.** blurter is a plain HTTPS call. Measured on
2026-07-26, a headless `claude` had its Slack connector available in 1 of 6
invocations — disqualifying for something that must work while other things are
broken. An agent may usefully read the audit log or submit events; it must not be
what delivers them.

**Transient versus permanent failures are distinguished.** No network or a Slack
5xx is retried with exponential backoff; `invalid_auth` or `missing_scope` is
not, because retrying a misconfiguration forever buries real alerts.

If you use an agentic coding tool, point it at
[`agents-guide.md`](agents-guide.md), or run `blurter --help-agent`.

## Licence

Apache 2.0. See [LICENSE](LICENSE).

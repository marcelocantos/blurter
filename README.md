# blurter

A local notification daemon. Applications **spool** events; blurter owns the
credentials, the delivery policy, and the sinks.

```
  your app ──► blurter send ──► spool/<id>.json      (instant, offline-safe)
                                      │
                     blurter daemon ───┴──► policy ──► Slack DM / channel
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
brew services start blurter
```

The formula ships a service definition, so `brew services` manages the daemon.
Without Homebrew, `blurter service install` generates and loads an equivalent
launchd agent, or run `blurter daemon` under `systemd --user` on Linux.

From source: `go build -o blurter .` (or `make build`, which stamps the
version). The banner sink shells out to `terminal-notifier`, falling back to
`osascript`; the Homebrew formula depends on `terminal-notifier`, a from-source
install should `brew install terminal-notifier` too.

There is one binary. `blurter daemon` *is* the daemon — `blurterd` survives only
as the launchd label `com.marcelocantos.blurterd` and its log filename.

## Configure

`~/.config/blurter/config.yaml`:

```yaml
slack:
  token_file: slack-token     # relative to this dir; mode must be 600
  dm: U01234567               # your Slack member ID
  # channel: C01234567        # used by apps with destination: channel

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
  noisy-batch:
    destination: channel       # dm (default) | channel | an explicit ID
```

Every `apps.<name>` key is also valid under `default`, and an app section
overrides only the fields it names. `destination` resolves three ways: `dm` (or
unset) uses `slack.dm`, `channel` uses `slack.channel`, and anything else is
taken as a literal channel or member ID. `renotify_after: 0s` disables
re-notification entirely.

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

`send` prints the path of the spooled event; pass `--quiet` to suppress that.

> `blurter send` exiting 0 means **spooled**, not **delivered**. Delivery is
> asynchronous by design. Use `blurter status` or the audit log to confirm.

## Ad-hoc messages from an agent session

blurter is an MCP server, so an agent can raise a notification directly —
useful for "tell me when this finishes" on a long task:

```sh
claude mcp add --scope user blurter -- blurter mcp
```

Equivalently, in an MCP client's JSON config:

```json
{
  "mcpServers": {
    "blurter": { "command": "blurter", "args": ["mcp"] }
  }
}
```

The surface is three tools, in full:

| Tool | Parameters |
|------|------------|
| `blurter_send` | `subject` (required), `body`, `severity` (`problem`\|`ok`\|`info`, default `info`), `app` (default `adhoc`), `key`, `link` |
| `blurter_status` | none |
| `blurter_recent` | `limit` (default 20), `app` |

Note what the MCP surface deliberately does **not** do: deliver. The tools spool
and read, exactly like the CLI. Delivery stays in the daemon, with no LLM in
that path — an agent asking for a message is a submitter like any other app, and
gets the same durability and policy.

## Inspect

```sh
blurter status     # spool depth, per-app state, sink readiness, config paths
blurter drain      # deliver what's spooled now, then exit
```

Service management (launchd, macOS only): `blurter service install`,
`service uninstall`, `service path`, and `service install --dry-run` to print
the generated plist without writing it.

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
5xx is retried with exponential backoff (30s doubling, capped at 15 minutes);
`invalid_auth` or `missing_scope` is not, because retrying a misconfiguration
forever buries real alerts. An event is parked on the first permanent failure,
or after 12 transient attempts. The attempt count is held in the running
process, so it restarts from zero when the daemon does — deliberate, since a
restart usually follows a fix.

If you use an agentic coding tool, point it at
[`agents-guide.md`](agents-guide.md), or run `blurter --help-agent`.

## Licence

Apache 2.0. See [LICENSE](LICENSE).

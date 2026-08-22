# Entropy audit — blurter — 2026-08-22

Full audit (entropy + hygiene). First report for this repository; no prior
baseline.

## Executive summary

- **Snapshot:** `/Users/marcelo/work/github.com/marcelocantos/blurter`
  (`github.com/marcelocantos/blurter`), branch `master`,
  HEAD `14bdd71b724be9766f962c4906577150e2cae48f`
  (`14bdd71 Update bullseye.yaml`, 2026-07-27).
- **Initial dirty state** (before this report): `?? .claudia-mcp-home/`
  (user-owned, untracked; excluded from analysis). Working tree otherwise
  clean and aligned with `origin/master`.
- **Date:** 2026-08-22.
- **Scope:** all tracked Go, Makefile, CI/release workflows, product docs,
  `bullseye.yaml`. Single module, ~2.8k lines of Go including tests.
- **Exclusions:** untracked `.claudia-mcp-home/` (Claudia/Grok session cache,
  not product code). No vendored or generated trees in-repo. Homebrew formula
  lives in `marcelocantos/homebrew-tap` and was read only as a competing
  service definition.
- **Headline mechanism:** submit-versus-deliver is a clean, enforced split;
  entropy is in the *operator* and *policy* seams. Status and service install
  each have two implementations that have already drifted, and retry/recovery
  key off the wrong clock and the wrong severity rank — so the properties the
  spool was built to protect (offline retry, recovery notices) fail under
  documented configurations and aged events.
- **Highest-consequence findings:**
  - ENT-001 (P1): backoff is measured from `Event.CreatedAt`, so an event that
    sat in the spool during an outage is retried every poll tick until
    `maxAttempts` and then parked.
  - ENT-002 (P1): `min_severity` is applied before recovery, and `ok` ranks
    below `problem`, so the README's `min_severity: problem` example silently
    disables `RECOVERED` notices.
- **Unverified residue:** live Slack/`terminal-notifier` delivery; ytt
  migration claimed by 🎯T2; whether `blurter service install` is used on
  Homebrew binaries in the wild; `govulncheck` (tool not runnable here).

## Scope and exclusions

Tracked production surface is one `package main` module:

| Path | Role |
|---|---|
| `main.go` | CLI dispatch: send, daemon, drain, status, service, mcp |
| `event.go` | Event, digest, validate, atomic spool |
| `config.go` | XDG paths, YAML config, token mode check |
| `policy.go` | Decide, per-app state |
| `daemon.go` | Drainer, backoff, park, audit |
| `sinks.go` | Slack HTTPS + desktop banner |
| `service.go` | launchd plist generate/load |
| `mcp.go` | MCP stdio tools (send/status/recent) |
| `*_test.go` | 39 tests |
| `.github/workflows/ci.yml`, `release.yml` | CI + Homebrew release |
| `README.md`, `agents-guide.md` | human vs agent docs (guide is `//go:embed`) |
| `bullseye.yaml` | 🎯T1 (this repo) achieved; 🎯T2 (ytt) achieved |

Excluded: `.claudia-mcp-home/` (untracked). No `vendor/`, no generated code,
no `hygiene.yaml`, no `AGENTS.md`/`CLAUDE.md`.

## Commands run

| Command | Version / notes | Exit | Shipped path? | Limitation |
|---|---|---|---|---|
| `git status --porcelain=v1 -b`; `git rev-parse HEAD` | git 2.55.0 | 0 | n/a | Initial snapshot. Branch `master...origin/master`. Dirty: `?? .claudia-mcp-home/` only. |
| `git log --oneline -20`; `git tag -l` | | 0 | n/a | 5 commits; tag `v0.1.0`. |
| `gofmt -l .` | gofmt from go1.26.4 | 0, empty | yes (`make lint` / CI `gofmt`) | |
| `go vet ./...` (GOWORK inherited) | go1.26.4 | 1 | intended yes | Fails: parent `/Users/marcelo/work/github.com/marcelocantos/go.work` lists only `claudia` and `jevons`. |
| `go test ./... -count=1 -race` (inherited GOWORK) | | 1 | intended yes | Same workspace failure. |
| `GOWORK=off go vet ./...` | go1.26.4 | 0 | local stand-in for CI | CI runners have no parent `go.work`. |
| `GOWORK=off go test ./... -count=1 -race -timeout 90s` | go1.26.4 | 0, `ok` 1.779s | local stand-in | 39 tests passed. |
| `GOWORK=off go test ./... -count=1 -coverprofile=...` | | 0 | auxiliary | **50.1%** statements. `BannerSink.Deliver` 0%; `cmdSend`/`cmdStatus`/`cmdDaemon`/`cmdDrain`/`cmdService`/`buildDrainer`/`Run`/`DefaultPaths` 0%. `Decide` 100%; `DrainOnce` 87.8%; Slack `Deliver` 74.4%. |
| `GOWORK=off go build` + `--version`/`--help`/`--help-agent` | | 0 | yes (CI CLI contract) | `--help-agent` 197 lines (usage + embedded guide). |
| `GOWORK=off make lint vet test` | GNU Make 3.81 | 0 | yes (Makefile) | `make test` is `-count=1` without `-race`; CI adds `-race`. |
| `GOWORK=off go list -deps` | | 0 | n/a | Non-stdlib: `gopkg.in/yaml.v3` only. |
| `staticcheck ./...` | `/Users/marcelo/go/bin/staticcheck` built with go1.25 | 1 | auxiliary | Unusable: `file requires newer Go version go1.26`. |
| `govulncheck` | not installed | n/a | n/a | Residue. |
| `gh run list`; `gh repo view`; branch protection; security settings | gh | 0 | CI history | Last CI on `master`: success, 2026-07-26. Branch **not protected**. Secret scanning + push protection enabled. Dependabot security updates **disabled**. Vulnerability alerts **disabled**. |
| `gh release view v0.1.0` | | 0 | release artifacts | darwin-arm64, linux-amd64, linux-arm64 tarballs. |
| Homebrew formula `Formula/blurter.rb` via `gh api` | tap `marcelocantos/homebrew-tap` | 0 | shipped brew path | Read as competing service SoT. Formula **does** carry sha256 despite `skip_checksum: true` in the workflow. |
| `~/.claude/skills/hygiene/hygiene_check.py` | not run | n/a | n/a | No `hygiene.yaml`; brief forbids initializing. |

`GOWORK=off` is **not** the Makefile/CI command. The declared local oracle
(`make test` → `go test ./...`) fails on this machine without it.

## Observed architecture

One binary, two runtimes, one spool contract.

```
apps / agents
  ├─ `blurter send`  ──► Event.Validate + Event.Spool (no config, no token, no net)
  └─ MCP `blurter_send` ──► same spool
                              │
                              ▼
                     $XDG_STATE_HOME/blurter/spool/*.json
                              │
`blurter daemon` / `drain` / brew service
  └─ Drainer.DrainOnce
        ├─ Config.PolicyFor + Decide          (pure)
        ├─ Sink.Deliver (Slack, Banner)       (only net/exec)
        ├─ State.Record *after* success
        ├─ audit.jsonl append
        └─ remove or parked/
```

**Declared rules that match the code**

- Submitting does not load credentials or touch the network (`cmdSend` at
  `main.go:107-157` never calls `LoadConfig` / `SlackToken`).
- Config is XDG, never next to the executable (`config.go:26-36`).
- Token file mode must be `0600` or stricter (`config.go:210-212`).
- State is recorded only after a sink confirms (`daemon.go:120-125`).
- Undeliverable events are parked, not deleted (`daemon.go:198-209`).
- MCP tools spool and read; they do not deliver (`mcp.go:18-25`, `toolSend`).
- Slack treats HTTP 200 `ok:false` as failure (`sinks.go:104-125`).

**Observed rules inferred from code (not separately documented as such)**

- Single `package main`; layering is by file, not by import graph. Appropriate
  at this size; there is no forbidden-edge test because there are no packages
  to police.
- Delivery is "any sink succeeds" (`daemon.go:158-176`). Banner default-on
  means Slack can fail permanently while the event is still marked delivered.
- Backoff origin is `CreatedAt`, not last attempt (`daemon.go:91-96`).
- `min_severity` is a numeric rank check that includes `ok` (`event.go:46-56`,
  `policy.go:109-111`).
- Two operator status formatters (`cmdStatus`, `toolStatus`); two service
  installers (`service.go` launchd, `release.yml` Homebrew `service` block).

**Contradictions**

- `agents-guide.md:157-160` tells operators to check `blurter status` for a
  growing `parked/`. `cmdStatus` does not mention parked; only MCP status does.
- Homebrew `opt_bin/"blurter"` survives upgrades; `service install`
  `EvalSymlinks` pins a Cellar path. README calls the generated agent
  "equivalent" (`README.md:39-41`) while log path and PATH already differ.
- `deliver`'s comment (`daemon.go:158-160`) says retryable only if *no* sink
  permanently rejected; the return is `anyRetryable` from any sink.

**Unknown intent**

- Whether `ok` should outrank `min_severity` so recovery always fires.
- Whether `blurter service install` is meant to be used on Homebrew binaries
  (comment at `service.go:98-100` acknowledges the symlink and then undoes it).

## Dimension vector

| Dimension | State | Evidence summary | Change from baseline |
|---|---|---|---|
| Architecture topology | healthy | One module, one spool, submitters never depend on sinks. Direction is send → spool → drain → policy → sinks. No cycles. | n/a (first audit) |
| Redundancy / sources of truth | concern | Status (CLI vs MCP) and service install (launchd vs brew formula) already disagree. README vs agents-guide overlap is mostly deliberate. | n/a |
| Change amplification | concern | A status field or service PATH/log change must be made in two places that have already drifted. Policy/spool changes stay local. | n/a |
| Local code quality | healthy | Linear, commented, no options-pattern, no needless packages. Defects below are logic, not clutter. | n/a |
| Correctness / verification | concern | Original ytt-class bugs have tests. Backoff clock, min_severity vs recovery, BannerSink, and the CLI wiring path do not. 50.1% coverage; CI green. | n/a |
| Security / dependencies | concern | Tight token handling and secret-scan/push-protection; no vuln alerts, no Dependabot, no `govulncheck` in CI. One dependency (`yaml.v3`). | n/a |
| Build / release / operations | concern | CI macOS+Linux race+gofmt+vet+CLI flags. Release cross-builds and updates the tap. Local `make test` broken under sibling `go.work`. Two service defs. | n/a |
| Documentation / governance | concern | Product docs are strong; `hygiene.yaml` absent; master unprotected; 🎯T2 (ytt) is achieved in this ledger with no in-repo evidence. | n/a |

## Findings

### ENT-001: Retry backoff is keyed off event creation time, so aged spool entries burn `maxAttempts` and park

- **Priority:** P1
- **Dimensions:** Correctness / verification; Architecture topology
- **Status:** observed fact
- **Evidence:**
  - `daemon.go:91-96` defers only while `now.Sub(e.CreatedAt) < backoff(n-1)`.
  - `daemon.go:55-68`: `backoff(0)=30s` … cap 15m; `maxAttempts=12`.
  - `daemon.go:251-262`: `Run` drains immediately then every `--interval`
    (default 5s, `main.go:195`).
  - `daemon.go:31-34`: `attempts` is in-memory and dropped on restart.
  - `drain_test.go:112-142` (`TestTransientFailureRetainsEventAndDoesNotRecordState`)
    jumps `Now` by one hour and asserts a later success; it never asserts
    deferral, and does not cover an event whose `CreatedAt` is already past
    the cap.
  - No test named for backoff. `DrainOnce` coverage 87.8% (the defer branch
    is the obvious gap).
- **Mechanism:** An event raised while the machine is offline has a
  `CreatedAt` hours in the past. When Slack is still 5xx (or the network is
  flapping), every 5s drain sees `now - CreatedAt` already greater than
  `backoff(n-1)`, so it never defers. Twelve attempts later
  (`daemon.go:136-144`) the event is **parked** as "gave up after 12
  attempts". Parked files are not retried. The spool's at-least-once promise
  degrades to "at most ~60s of rapid retries after a long wait, then human
  salvage."
- **Blast radius:** Every event that ages in the spool before a transient
  failure clears — the outage case T1 exists to handle
  (`bullseye.yaml` T1 acceptance: retry transient failures; deliver later
  rather than drop). Banner-enabled Macs often still "deliver" via
  `TestDeliveryContinuesWhenOneSinkFails` semantics; Linux / banner-off /
  Slack-only setups park the alert.
- **Counterevidence checked:** Restart resetting `attempts` is deliberate
  (`daemon.go:31-34`) and is a *retry immediately* choice, not a fix for the
  clock. Permanent failures still park on the first attempt
  (`drain_test.go:144-161`). Events are not deleted. Comment on `backoff`
  claims it caps so a long outage does not push the interval past usefulness
  — the `CreatedAt` origin inverts that for already-old events.
- **Smallest coherent remediation:** Keep attempt count in memory as
  `{n, lastAttempt}` and compare `now.Sub(lastAttempt)` to `backoff(n-1)`.
  Do not rewrite spool files. On restart, empty map ⇒ retry immediately
  (preserve the documented restart behaviour).
- **Verification:** Drain an event with `CreatedAt = now-2h`, sink always
  transient-fail, advance `Now` by 5s per `DrainOnce` for 12 calls: event
  must remain in the spool (deferred), not in `parked/`. A regression parks
  it.
- **Ratchet candidate:** that test in CI; optionally an architecture comment
  next to `attempts` that the clock is last-attempt, not created-at.

### ENT-002: `min_severity` is applied before recovery, and `ok` ranks below `problem`

- **Priority:** P1
- **Dimensions:** Correctness / verification; Redundancy / sources of truth
  (docs example vs engine)
- **Status:** observed fact
- **Evidence:**
  - Rank order `info=0`, `ok=1`, `problem=2` (`event.go:46-56`).
  - `Decide` drops on `e.Severity.rank() < min.rank()` *before* the recovery
    branch (`policy.go:109-125`).
  - README documents the motivating config as dropping *info*
    (`README.md:64-65`: `chatty-app: min_severity: problem  # drop its info events`).
    Same example in `agents-guide.md:103-104`.
  - `TestDecideMinSeverityDropsInfo` (`policy_test.go:124-129`) only sends
    `info`. No test that `ok` still recovers under `min_severity: problem`.
  - Amplifier: `DrainOnce` calls `RecordSuppressed` for every non-delivery
    (`daemon.go:101-107`), and `RecordSuppressed` overwrites `LastStatus`
    (`policy.go:167-177`). A dropped `info` therefore also clears
    `LastStatus=problem`, so even a later un-filtered `ok` would not recover.
- **Mechanism:** For the documented `min_severity: problem` app, `ok` has
  rank 1 < 2, so recovery is suppressed as "below configured minimum". The
  human gets problems and never a `RECOVERED` notice. T1 acceptance requires
  per-app recovery notices (`bullseye.yaml` T1).
- **Blast radius:** Any app section with `min_severity: problem` (the
  documented way to quiet a chatty app). Mute is a separate, explicit
  silence; this one is accidental.
- **Counterevidence checked:** `TestDecideRecoveryNoticeSentOnce`
  (`policy_test.go:94-115`) uses a policy *without* `MinSeverity`, so it
  cannot catch this. Default `PolicyFor` fills `MinSeverity=info`
  (`config.go:130-132`), which does not drop `ok`. Pure `Decide` is otherwise
  well tested (100% on that function).
- **Smallest coherent remediation:** Apply `min_severity` only to `info`
  (or skip the check when `e.Severity==ok` and a problem is outstanding).
  Stop calling `RecordSuppressed` for below-min events, or do not let those
  events change `LastStatus`. Align the README comment with the rule.
- **Verification:** `Decide(ok, AppPolicy{MinSeverity: problem, ...}, prevProblem, now)`
  must `Deliver` with a `RECOVERED` subject. Drain path: problem delivered,
  info suppressed, ok must still recover. Regression: ok suppressed with
  reason `below configured minimum`.
- **Ratchet candidate:** the two tests above in `policy_test.go` /
  `drain_test.go`.

### ENT-003: CLI `status` and MCP `blurter_status` are competing operator views; only MCP reports `parked/`

- **Priority:** P2
- **Dimensions:** Redundancy / sources of truth; Change amplification;
  Documentation / governance
- **Status:** observed fact
- **Evidence:**
  - `cmdStatus` (`main.go:224-272`): config/state/spool paths, spool depth,
    slack/banner, apps. **No `parked/`.**
  - `toolStatus` (`mcp.go:293-338`): spool depth, slack/banner, **parked
    count** (`mcp.go:321-325`), apps. No paths.
  - `agents-guide.md:157-160`: "A growing `parked/` means a permanent
    misconfiguration — check `blurter status` and the audit log."
  - `TestMCPStatusReportsSpoolAndSinks` does not assert parked.
  - `cmdStatus` is 0% covered.
- **Mechanism:** Parked events are the terminal failure the product refuses
  to delete. The command operators are told to run cannot see them. MCP and
  CLI will keep drifting on the next status field (they already differ on
  paths vs parked).
- **Blast radius:** Anyone diagnosing "blurter ate it" with the CLI; future
  status fields.
- **Counterevidence checked:** Audit log still records `parked:` outcomes
  (`daemon.go:142`). MCP `blurter_recent` can answer after the fact. That
  does not make `status` match the guide.
- **Smallest coherent remediation:** One status renderer used by CLI and
  MCP. Include parked count (and path) in both.
- **Verification:** Tempdir with one file in `state/parked/`; `blurter status`
  and `blurter_status` both mention parked. Regression: CLI output lacks
  `parked`.
- **Ratchet candidate:** shared helper + table test of required keys
  (`spool`, `slack`, `banner`, `parked`).

### ENT-004: Two service installers already disagree on binary path, logs, and PATH

- **Priority:** P2
- **Dimensions:** Redundancy / sources of truth; Build / release / operations;
  Change amplification
- **Status:** observed fact (EvalSymlinks); inference (Homebrew upgrade break
  for `service install`)
- **Evidence:**
  - `service.go:98-118`: `filepath.EvalSymlinks` on `os.Executable()`, log
    `~/.local/var/log/blurter/blurterd.log`, PATH hardcoded
    `/opt/homebrew/bin:/usr/local/bin:...`.
  - Comment at `service.go:98-100` notes the Homebrew `bin/` symlink
    retargets on upgrade "which is fine", then resolves it away.
  - `release.yml:111-118` (and the generated tap formula): `run
    [opt_bin/"blurter", "daemon"]`, log `var/"log/blurter.log"`, PATH from
    `HOMEBREW_PREFIX`.
  - README (`README.md:36-41`) scopes `blurter service install` to
    *without Homebrew*, as an "equivalent" launchd agent. The generated
    plist is not actually equivalent: different log path, different PATH
    construction, and `EvalSymlinks` vs `opt_bin`.
  - `cmdService` is 0% covered; no dry-run golden test.
- **Mechanism:** `EvalSymlinks` on a Homebrew binary yields
  `/opt/homebrew/Cellar/blurter/<version>/bin/blurter`. `brew upgrade`
  removes that Cellar path; launchd keeps pointing at it. The formula's
  `opt_bin` is the stable path. Logs live in two places, so "the daemon is
  silent" has two hunt locations. The documented "without Homebrew" split
  does not stop `blurter service install` from being run against a brew
  binary on `$PATH`.
- **Blast radius:** Anyone who runs `service install` on a Homebrew
  `blurter`. From-source installs with a stable path are fine. Every future
  PATH/log/KeepAlive change must be edited twice.
- **Counterevidence checked:** Formula comment in `release.yml:99-101`
  correctly refuses hand-edits in the tap. `service.go` generation is the
  right idea for non-brew machines. The defect is resolving the symlink,
  not generating the plist.
- **Smallest coherent remediation:** Do not `EvalSymlinks` when the
  executable path is already a stable prefix (`/opt/homebrew/bin`,
  `/usr/local/bin`, `~/.local/bin`). Keep resolution for ad-hoc `go build`
  paths if diagnosis needs it. Document that Homebrew users must use
  `brew services`. Consider generating log path under XDG state to collapse
  the third location (`~/.local/var/log` vs XDG state vs Homebrew `var/log`).
- **Verification:** `blurter service install --dry-run` with a stub
  executable under a fake `opt/homebrew/bin` symlink: plist `ProgramArguments`
  must name the symlink, not the Cellar target.
- **Ratchet candidate:** `--dry-run` test asserting ProgramArguments and
  PATH.

### ENT-005: Unreadable spool files are logged forever and never parked

- **Priority:** P2
- **Dimensions:** Correctness / verification; Local code quality
- **Status:** observed fact
- **Evidence:**
  - `readSpool` returns parse/validate failures in `errs` and omits them
    from `events` (`event.go:201-214`).
  - `DrainOnce` logs those errors and otherwise ignores them
    (`daemon.go:77-82`). The file stays in the spool directory.
  - `TestUnparseableSpoolFileIsReportedNotIgnored` (`drain_test.go:230-238`)
    only checks `readSpool` returns an error, not that `DrainOnce` parks or
    isolates the file.
- **Mechanism:** A corrupt `*.json` produces `SPOOL ERROR` every poll
  (default 5s) until a human deletes it. It is never delivered, never
  parked, never surfaced in CLI status (ENT-003). The project's own rule —
  never silently skip an unreadable spool file — is met as a log line and
  then abandoned.
- **Blast radius:** One bad file: log spam + a "pending" count that does
  not include it (`readSpool` skips it, so `status` pending can read 0
  while the directory is not empty).
- **Counterevidence checked:** Temp files prefixed `.` are skipped
  (`event.go:190-191`), which is correct. Invalid events that fail
  `Validate` in `send` never reach the spool (`main.go:143-147`,
  `TestMCPSendRejectsMissingSubject`). This finding is daemon-side residue
  of a write that passed `send` then rotted, or an external write.
- **Smallest coherent remediation:** On read/parse/validate error, `park`
  the file (same as permanent delivery failure) and audit `parked: unreadable`.
- **Verification:** Write `{ not json` as `spool/000.json`, `DrainOnce`,
  assert spool empty, `parked/` has the file, audit contains `unreadable`.
- **Ratchet candidate:** extend `TestUnparseableSpoolFileIsReportedNotIgnored`
  to run through `DrainOnce`.

### ENT-006: Load-bearing owner surfaces are untested on the shipped path

- **Priority:** P2
- **Dimensions:** Correctness / verification
- **Status:** observed fact
- **Evidence:**
  - Coverage 50.1%. Zero: `BannerSink.Deliver` / `shellQuote` /
    `appleScriptString` (`sinks.go:154-209`, injectable `LookPath`/`Run`
    unused by tests); `cmdSend`, `buildDrainer`, `cmdDaemon`, `cmdDrain`,
    `cmdStatus`, `cmdService`, `Run`, `DefaultPaths`, `xdgDir`,
    `UnmarshalYAML`.
  - CI CLI contract (`.github/workflows/ci.yml:28-33`) runs `--version`,
    `--help`, `--help-agent` only.
  - Slack is covered via `httptest` (`drain_test.go:264-328`). Banner is
    not. No test that `blurter send` avoids `SlackToken` (true by inspection
    of `main.go:107-157`, not locked).
  - No journey: send → real daemon → banner or Slack, machine-checkable.
    T1's "CI green with vet/gofmt/-race" is held; T1's delivery properties
    are held only for injected `fakeSink`.
- **Mechanism:** The original ytt hole is tested (`TestTransientFailure...`).
  ENT-001 and ENT-002 survived because the untested corners are exactly
  backoff, min_severity×ok, banner, and CLI wiring. Coverage percentage is
  not the finding; the missing properties are.
- **Blast radius:** Banner quoting (`-execute open ` + `shellQuote`) and
  AppleScript string escaping are user-controlled text on an exec boundary
  with no test. CLI regressions in `buildDrainer` sink selection would not
  fail CI.
- **Counterevidence checked:** 39 tests, all passing under
  `GOWORK=off go test -race`. Policy engine and Slack `ok:false` handling
  are genuinely strong. MCP protocol tests exercise real stdio framing.
- **Smallest coherent remediation:** (1) BannerSink tests with fake
  `LookPath`/`Run` for both tools and quoting. (2) `cmdSend` test:
  does not open network, writes spool. (3) Tests for ENT-001/002/003/005.
  A live Slack journey is residue (needs a workspace), not a substitute for
  (1)–(3).
- **Verification:** coverage on `BannerSink.Deliver` > 0 and the new tests
  red on the current bugs.
- **Ratchet candidate:** CI `-coverprofile` with a file-list floor on
  `sinks.go` / `main.go` cmd paths, or explicit tests named above. Do not
  ratchet a global 50% → 80% number.

### ENT-007: Declared local test command fails in the owner's Go workspace

- **Priority:** P3
- **Dimensions:** Build / release / operations; Correctness / verification
- **Status:** observed fact
- **Evidence:** Parent `/Users/marcelo/work/github.com/marcelocantos/go.work`
  (`use ( ./claudia ./jevons )`). `go env GOWORK` from this repo points at
  it. `go test .` without `GOWORK=off` exits 1: "current directory is
  contained in a module that is not one of the workspace modules". Makefile
  `test` is `go test ./... -count=1` (`Makefile:12-13`). CI does not set
  `GOWORK=off` and is green because the runner has no parent workspace.
- **Mechanism:** The shipped local oracle is not the command that actually
  passes on the machine that owns the repo. Agents and humans running
  `make test` get a workspace error, not a product signal.
- **Blast radius:** Local verification only. CI still decides merges.
- **Counterevidence checked:** `GOWORK=off make test` passes. This is not a
  blurter `go.mod` bug.
- **Smallest coherent remediation:** `GOWORK=off` on Makefile `test`/`vet`
  recipes (and matching CI `env:` for belt-and-braces). Alternatively add
  this module to the parent workspace — that change is outside this repo.
- **Verification:** `make test` with `GOWORK` pointing at the sibling
  workspace exits 0.
- **Ratchet candidate:** Makefile recipes prefix `GOWORK=off`.

### ENT-008: Supply-chain and merge gates are thin for a credential-owning daemon

- **Priority:** P3
- **Dimensions:** Security / dependencies; Build / release / operations;
  Documentation / governance
- **Status:** observed fact
- **Evidence:**
  - Dependabot security updates disabled; vulnerability alerts disabled
    (`gh api` on the repo). No `.github/dependabot.yml`. No `govulncheck`
    job. `govulncheck` not installed locally.
  - `master` branch protection 404. Combined status on HEAD: `pending`,
    `total_count: 0` required checks.
  - CI *does* run on push/PR and was green for every `master` push listed.
  - Secret scanning and push protection **enabled**. `.gitignore` ignores
    `slack-token`, `*.token`, `.env`. Token mode check tested
    (`drain_test.go:331-360`).
  - `release.yml:17-18` `permissions: contents: write` on the whole
    workflow including matrix build. `skip_checksum: true`
    (`release.yml:97`) — published formula nevertheless contains sha256
    for each asset (tap `Formula/blurter.rb`).
  - No darwin/amd64 release asset (`release.yml:26-32`); owner host is
    darwin/arm64.
- **Mechanism:** A Slack bot token is the crown jewel of this process.
  Runtime handling is careful; the GitHub-side update/vuln loop is off, and
  merges are not mechanically blocked on CI. The risk is stale `yaml.v3` or
  a force-push of red CI, not a present leak.
- **Blast radius:** One Go module, one third-party dependency. Low until
  yaml.v3 or the Go stdlib HTTP stack has a CVE.
- **Counterevidence checked:** Apache-2.0 LICENSE, README, `.gitignore`,
  `go.sum` present. Release dry-run checkout bug already fixed (`8a11338`).
- **Smallest coherent remediation:** Enable vulnerability alerts +
  Dependabot for `gomod`; add a `govulncheck` CI step; require the existing
  `test` job on `master`. Leave `skip_checksum` unless homebrew-releaser
  needs it — artifacts are checksummed in the formula today.
- **Verification:** `gh api .../vulnerability-alerts` not 404; a PR with
  failing tests cannot merge.
- **Ratchet candidate:** hygiene items (once declared) for `security.vuln-scan`
  and `vcs.branch-protection`.

## Redundancy and competing-source-of-truth inventory

| Fact | Authorities | Drift already? |
|---|---|---|
| Operator health | `cmdStatus` vs `toolStatus` | Yes — parked (ENT-003) |
| How the daemon is installed | `service.go` launchd vs `release.yml` / tap `service do` | Yes — binary, log, PATH (ENT-004) |
| How to send an event | CLI `cmdSend` vs MCP `toolSend` | Defaults differ on purpose (CLI requires `--app`/`--severity`; MCP defaults `adhoc`/`info`). Both `Validate`+`Spool`. Acceptable duplication. |
| Human vs agent docs | `README.md` vs embedded `agents-guide.md` | Mostly aligned; parked→status is the live contradiction. |
| Retry / recovery policy | `Decide` + `DrainOnce` vs T1 / README examples | Yes — ENT-001, ENT-002 |
| Version | ldflags `main.version` vs git tag vs Homebrew `version` | Release workflow strips `v` consistently. |
| 🎯T2 "ytt notifies exclusively through blurter" | this `bullseye.yaml` (achieved) vs ytt repo | Cannot verify here. |

Deliberate duplication worth keeping: CLI vs MCP send (different defaults,
same spool); README vs agents-guide (different audiences, guide is embedded
so `--help-agent` cannot rot relative to the file).

## Healthy structure worth retaining

- **Spool as the contract.** `Event.Spool` is temp + `Sync` + rename
  (`event.go:127-162`). Submitters cannot mark delivered. This is the
  architecture; do not introduce a synchronous send path.
- **Pure `Decide`.** Policy is unit-testable without sinks; state mutation
  is a separate `Record` / `RecordSuppressed` (`policy.go:101-177`). Keep it
  that way when fixing ENT-002.
- **Per-app digest and state.** `TestDigestIsScopedPerApp`,
  `TestStateRecordOnlyOnSuccess` lock the isolation T1 requires.
- **Slack `ok:false` is not success.** `TestSlackSinkTreatsOkFalseAsFailure`
  plus permanent vs 5xx split (`sinks.go:97-125`).
- **Token mode 600.** Loud refusal with a `chmod 600` hint
  (`config.go:210-212`).
- **XDG, not argv0.** `DefaultPaths` comment (`config.go:26-30`) records the
  ytt fifteen-day outage; do not "simplify" to executable-relative config.
- **MCP does not deliver.** Tools are submit/inspect. Protocol tests cover
  initialize echo, notification silence, `isError` vs JSON-RPC errors.
- **Missing config is not fatal.** Banner-only fallback
  (`config.go:161-163`, `main.go:172-188`).
- **Corrupt `state.json` is not fatal.** Duplicate notification preferred
  over dropping all (`policy.go:33-52`, `TestCorruptStateFallsBackToEmpty`).
- **CI matrix + CLI contract + gofmt.** `.github/workflows/ci.yml`.
- **Release dry-run checks out the dispatched ref**, not a not-yet-existing
  tag (`release.yml:35-41`, commit `8a11338`).
- **No functional-options API**; config is a struct. Matches `go.md`.
- **Flat `package main`.** Splitting into internal packages now would be
  ceremony. Revisit only if a second binary appears.

## Hygiene posture

**Hygiene posture not declared.** No `hygiene.yaml` at repo root. Validator
not run; file not created.

Observed reality (not a hygiene vector — for a future declaration only):

| Dim | What exists | Gaps vs a typical floor 2 |
|---|---|---|
| correctness | `make test`, CI `go test -race` on ubuntu+macos | ENT-001/002 untested; no journey |
| quality | `gofmt` in Makefile+CI, `go vet` | no staticcheck/golangci in CI |
| security | secret scanning, push protection, token mode test | no vuln scan, Dependabot off |
| deps | `go.sum`, single dep | no Dependabot |
| release | `release.yml`, tag `v0.1.0`, Homebrew tap | `contents: write` on whole workflow |
| docs | README, LICENSE, agents-guide, `--help-agent` | no CHANGELOG |
| vcs | `.gitignore`, default branch `master` | branch unprotected |
| build | Makefile `build`/`install`, CGO=0 cross-compile | local `make test` vs `go.work` (ENT-007) |
| governance | `bullseye.yaml` | no CODEOWNERS, no `hygiene.yaml` |
| agent | `agents-guide.md` embed | no repo `AGENTS.md` (the guide is the product-facing substitute) |

Entropy findings suitable as future hygiene evidence: ENT-001/002 tests
(`command:`), ENT-003 status keys (`command:`), ENT-004 dry-run plist
(`command:`), ENT-007 `GOWORK=off` (`make_target:`), ENT-008 vuln scan
(`ci_job:`). Do not ratchet until those tests exist.

## Oracle coverage and residue

| Property | Decided by | Notes |
|---|---|---|
| `send` does not read credentials or block on the network | inspection of `cmdSend`; **not** a test | T1 acceptance #1 |
| Atomic spool (temp+fsync+rename) | `TestSpoolWriteIsAtomic` + `Event.Spool` code | fsync itself not failed-on |
| State only after sink success | `TestTransientFailureRetainsEventAndDoesNotRecordState` | shipped-path equivalent via `Drainer`, not CLI |
| Transient vs permanent Slack errors | httptest tests | not live Slack |
| Park on permanent failure | `TestPermanentFailureParksRatherThanDeletes` | |
| Dedup / renotify / recovery / mute / key | `policy_test.go` (pure `Decide`) | recovery×min_severity **not** decided (ENT-002) |
| Retry backoff | **nothing** | ENT-001 |
| Banner delivery / quoting | **nothing** | ENT-006 |
| CLI send/status/service | CI flags only | ENT-003/004/006 |
| `--version` `--help` `--help-agent` | CI CLI contract | held |
| gofmt, vet, -race on macOS+Linux | CI | last green 2026-07-26; local race green with `GOWORK=off` |
| MCP framing / tools | `mcp_test.go` | |
| Live Slack DM / desktop banner | **nothing** | needs a workspace; T2 claims a ytt E2E in another repo |
| Token file not committed | `.gitignore` + secret scanning | |
| yaml.v3 vulns | **nothing** | `govulncheck` unavailable |
| ytt exclusive use of blurter (🎯T2) | **nothing in this repo** | |

Failed/skipped checks: inherited-GOWORK `go test`/`go vet` (fail);
`staticcheck` (toolchain mismatch); `govulncheck` (missing);
`hygiene_check.py` (no yaml).

**Owner residue (intent, not mechanical follow-up):**

1. Should `ok` bypass `min_severity` so recovery always fires for a quieted
   app, or should `min_severity: problem` mean "problems only, including no
   recoveries"? Code does the latter; README comment claims the former.
2. Is `blurter service install` supported on Homebrew binaries, or only for
   from-source installs?
3. Accept "any sink succeeds" even when Slack returns `invalid_auth` (banner
   still delivers, status still says slack ready)? Today that is the code.
4. 🎯T2 lives in this ledger and is `achieved`; confirm against ytt or move
   the target.

## Remediation sequence

1. **Lock the two policy bugs.** ENT-002 (min_severity vs `ok`) then ENT-001
   (backoff clock). Tests first; they fail on current HEAD. These are the
   oracles T1 thinks it already has.
2. **One status renderer; park unreadable spool files.** ENT-003 + ENT-005.
   Operators can then see parked from the CLI the guide already names.
3. **Service install: stop EvalSymlinks on brew prefixes; dry-run test.**
   ENT-004. Optionally document `brew services` as the only Homebrew path.
4. **BannerSink + `cmdSend` tests.** ENT-006. Cheap because LookPath/Run
   are already injected.
5. **Makefile `GOWORK=off`.** ENT-007.
6. **Declare `hygiene.yaml` at the floor this repo actually holds** (gofmt,
   vet, race CI, LICENSE, README, secret scan) and park vuln-scan /
   branch-protection as `planned` above the floor. Then ENT-008.
7. Re-run this audit against the same finding IDs.

Do not split the module, do not add a second send path, do not put an LLM
on delivery, do not inline the Slack token in YAML.

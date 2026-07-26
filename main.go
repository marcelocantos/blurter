// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command blurter is a local notification daemon. Applications spool events;
// blurter owns the credentials, the delivery policy, and the sinks.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

//go:embed agents-guide.md
var embedded embed.FS

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "blurter: %v\n", err)
		os.Exit(1)
	}
}

const usage = `blurter — local notification daemon.

Applications spool events; blurter owns the Slack credential, the delivery
policy (dedup, re-notify windows, recovery notices), and the sinks. Submitting
apps never see a credential and never block on the network.

Usage:
  blurter send --app NAME --severity LEVEL --subject TEXT [--body TEXT|-] [flags]
  blurter daemon [--interval DURATION]
  blurter drain            deliver everything spooled now, then exit
  blurter status           show spool depth, per-app state, and config paths
  blurter service install  write a launchd agent for the current user
  blurter --version | --help | --help-agent

Severity is one of problem, ok, info.

  send flags:
    --app NAME          submitting application (required)
    --severity LEVEL    problem | ok | info (required)
    --subject TEXT      one-line summary (required)
    --body TEXT         detail; "-" reads stdin
    --key TEXT          dedup key override, when the text varies but the
                        underlying problem does not
    --link PATH|URL     what the notification's click action opens

Exit status is 0 when the event was spooled, not when it was delivered:
delivery is asynchronous by design, so a submitting app is never blocked or
failed by a notification problem.
`

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return nil
	}

	switch args[0] {
	case "--version", "-V", "version":
		fmt.Fprintf(stdout, "blurter %s\n", version)
		return nil
	case "--help", "-h", "help":
		fmt.Fprint(stdout, usage)
		return nil
	case "--help-agent":
		// Usage first, then the agent guide: an agent calling this once gets
		// both the CLI reference and the domain guide.
		fmt.Fprint(stdout, usage)
		guide, err := embedded.ReadFile("agents-guide.md")
		if err != nil {
			return fmt.Errorf("reading embedded agent guide: %w", err)
		}
		fmt.Fprintf(stdout, "\n---\n\n%s", guide)
		return nil
	case "send":
		return cmdSend(args[1:], stdout)
	case "daemon", "serve":
		return cmdDaemon(args[1:], stderr)
	case "drain":
		return cmdDrain(args[1:], stderr)
	case "status":
		return cmdStatus(args[1:], stdout)
	case "service":
		return cmdService(args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q (try --help)", args[0])
	}
}

func cmdSend(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	app := fs.String("app", "", "submitting application (required)")
	severity := fs.String("severity", "", "problem | ok | info (required)")
	subject := fs.String("subject", "", "one-line summary (required)")
	body := fs.String("body", "", `detail; "-" reads stdin`)
	key := fs.String("key", "", "dedup key override")
	link := fs.String("link", "", "path or URL the click action opens")
	quiet := fs.Bool("quiet", false, "do not print the spooled event path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *body == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading body from stdin: %w", err)
		}
		*body = strings.TrimRight(string(data), "\n")
	}

	now := time.Now()
	id, err := newEventID(now)
	if err != nil {
		return err
	}
	e := &Event{
		ID:        id,
		App:       *app,
		Severity:  Severity(*severity),
		Subject:   *subject,
		Body:      *body,
		Key:       *key,
		Link:      *link,
		CreatedAt: now,
	}
	// Validate before spooling so a malformed event is the submitter's error,
	// reported synchronously, rather than a record the daemon cannot deliver.
	if err := e.Validate(); err != nil {
		return err
	}

	paths := DefaultPaths()
	path, err := e.Spool(paths.Spool)
	if err != nil {
		return err
	}
	if !*quiet {
		fmt.Fprintln(stdout, path)
	}
	return nil
}

// buildDrainer wires config, state and sinks together.
func buildDrainer(logw io.Writer) (*Drainer, error) {
	paths := DefaultPaths()
	cfg, err := LoadConfig(paths)
	if err != nil {
		return nil, err
	}
	state, err := LoadState(paths.State)
	if err != nil {
		return nil, err
	}

	var sinks []Sink
	token, tokenErr := cfg.SlackToken(paths)
	switch {
	case tokenErr != nil:
		// A misconfigured credential must be loud. Carrying on with only the
		// banner is right — dropping every alert because Slack is misconfigured
		// would be worse — but it must never be silent.
		fmt.Fprintf(logw, "blurter: slack disabled: %v\n", tokenErr)
	case token != "":
		sinks = append(sinks, &SlackSink{Token: token, Client: &http.Client{Timeout: 20 * time.Second}})
	}
	if cfg.Banner.enabled() {
		sinks = append(sinks, &BannerSink{})
	}
	if len(sinks) == 0 {
		fmt.Fprintf(logw, "blurter: WARNING no sinks configured; events will be parked undelivered\n")
	}

	return &Drainer{Paths: paths, Config: cfg, State: state, Sinks: sinks, Log: logw}, nil
}

func cmdDaemon(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	interval := fs.Duration("interval", 5*time.Second, "how often to drain the spool")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := buildDrainer(stderr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return d.Run(ctx, *interval)
}

func cmdDrain(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := buildDrainer(stderr)
	if err != nil {
		return err
	}
	delivered, deferred := d.DrainOnce(context.Background())
	fmt.Fprintf(stderr, "delivered %d, deferred %d\n", delivered, deferred)
	// Deferred events are not an error: the caller asked us to try, and a
	// retry is already scheduled.
	return nil
}

func cmdStatus(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := DefaultPaths()
	cfg, err := LoadConfig(paths)
	if err != nil {
		return err
	}
	state, err := LoadState(paths.State)
	if err != nil {
		return err
	}
	events, errs := readSpool(paths.Spool)

	fmt.Fprintf(stdout, "config: %s\n", filepath.Join(paths.Config, "config.yaml"))
	fmt.Fprintf(stdout, "state:  %s\n", paths.State)
	fmt.Fprintf(stdout, "spool:  %s (%d pending)\n", paths.Spool, len(events))
	for _, err := range errs {
		fmt.Fprintf(stdout, "  spool error: %v\n", err)
	}

	switch token, err := cfg.SlackToken(paths); {
	case err != nil:
		fmt.Fprintf(stdout, "slack:  unavailable (%v)\n", err)
	case token == "":
		fmt.Fprintf(stdout, "slack:  not configured (set slack.token_file)\n")
	case cfg.Slack.DM == "" && cfg.Slack.Channel == "":
		fmt.Fprintf(stdout, "slack:  token present but no destination (set slack.dm or slack.channel)\n")
	default:
		fmt.Fprintf(stdout, "slack:  ready\n")
	}
	fmt.Fprintf(stdout, "banner: %v\n", cfg.Banner.enabled())

	if len(state.Apps) == 0 {
		fmt.Fprintf(stdout, "\nno app state yet\n")
		return nil
	}
	fmt.Fprintf(stdout, "\napps:\n")
	for app, st := range state.Apps {
		last := "never"
		if !st.LastSentAt.IsZero() {
			last = st.LastSentAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout, "  %-16s status=%-8s last_sent=%s\n", app, st.LastStatus, last)
	}
	return nil
}

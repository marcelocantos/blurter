// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Drainer delivers spooled events. It is the only component that reads
// credentials or touches the network, which is the point of the whole design:
// submitting apps stay credential-free and never block.
type Drainer struct {
	Paths  Paths
	Config *Config
	State  *State
	Sinks  []Sink

	// Now is injected for tests.
	Now func() time.Time
	// Log receives operational messages.
	Log io.Writer

	// attempts counts failed delivery attempts per event ID, so backoff does
	// not require rewriting the spooled event. Lost on restart, which is
	// deliberate: a restart is a reasonable moment to retry immediately.
	attempts map[string]int
}

func (d *Drainer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Drainer) logf(format string, args ...any) {
	if d.Log == nil {
		return
	}
	fmt.Fprintf(d.Log, "[%s] "+format+"\n", append([]any{d.now().UTC().Format(time.RFC3339)}, args...)...)
}

// maxAttempts bounds retries for an event whose failures look transient but
// never clear. Past this the event is parked rather than deleted: losing an
// alert silently is the failure mode this project exists to prevent, so a human
// can still find it on disk.
const maxAttempts = 12

// backoff returns how long to wait before retrying an event with n prior
// failures, capped so a long outage does not push the retry interval past the
// point of usefulness.
func backoff(n int) time.Duration {
	const base = 30 * time.Second
	const cap = 15 * time.Minute
	d := time.Duration(math.Pow(2, float64(n))) * base
	if d > cap || d <= 0 {
		return cap
	}
	return d
}

// DrainOnce processes every currently-spooled event. Returns the number
// delivered and the number left for a later attempt.
func (d *Drainer) DrainOnce(ctx context.Context) (delivered, deferred int) {
	if d.attempts == nil {
		d.attempts = map[string]int{}
	}

	events, errs := readSpool(d.Paths.Spool)
	for _, err := range errs {
		// Never silent: an unreadable spool file is a bug, and pretending the
		// spool was empty is how alerts disappear.
		d.logf("SPOOL ERROR: %v", err)
	}

	stateDirty := false
	for _, se := range events {
		if ctx.Err() != nil {
			break
		}
		e := se.Event

		// Respect backoff for events that have already failed.
		if n := d.attempts[e.ID]; n > 0 {
			if d.now().Sub(e.CreatedAt) < backoff(n-1) {
				deferred++
				continue
			}
		}

		policy := d.Config.PolicyFor(e.App)
		decision := Decide(e, policy, d.State.ForEvent(e), d.now())
		if !decision.Deliver {
			d.logf("suppressed %s/%s: %s", e.App, e.Severity, decision.Reason)
			d.State.RecordSuppressed(e)
			stateDirty = true
			d.audit(e, decision, "suppressed", nil)
			d.remove(se.Path)
			continue
		}

		n := Notification{
			App:      e.App,
			Severity: e.Severity,
			Subject:  decision.Subject,
			Body:     e.Body,
			Link:     e.Link,
			Target:   d.target(policy),
		}

		ok, retryable, sinkErrs := d.deliver(ctx, n)
		if ok {
			// State is recorded only after a sink confirms. Recording on
			// attempt would let an outage mark an undelivered alert as sent,
			// and dedup would then suppress the retry — precisely the hole
			// that made ytt's own notifier lose offline alerts.
			d.State.Record(e, d.now())
			stateDirty = true
			delete(d.attempts, e.ID)
			d.logf("delivered %s/%s: %s", e.App, e.Severity, decision.Subject)
			d.audit(e, decision, "delivered", sinkErrs)
			d.remove(se.Path)
			delivered++
			continue
		}

		d.attempts[e.ID]++
		if !retryable || d.attempts[e.ID] >= maxAttempts {
			reason := "permanent failure"
			if retryable {
				reason = fmt.Sprintf("gave up after %d attempts", d.attempts[e.ID])
			}
			d.logf("PARKED %s/%s (%s): %v", e.App, e.Severity, reason, sinkErrs)
			d.audit(e, decision, "parked: "+reason, sinkErrs)
			d.park(se.Path)
			continue
		}
		d.logf("deferred %s/%s (attempt %d): %v", e.App, e.Severity, d.attempts[e.ID], sinkErrs)
		deferred++
	}

	if stateDirty {
		if err := d.State.Save(); err != nil {
			d.logf("STATE ERROR: %v", err)
		}
	}
	return delivered, deferred
}

// deliver attempts every sink. An event counts as delivered if any sink
// succeeds; it is retryable only if no sink permanently rejected it and at
// least one failure was transient.
func (d *Drainer) deliver(ctx context.Context, n Notification) (ok bool, retryable bool, errs []string) {
	anyRetryable := false
	for _, sink := range d.Sinks {
		sinkRetryable, err := sink.Deliver(ctx, n)
		if err == nil {
			errs = append(errs, sink.Name()+": ok")
			ok = true
			continue
		}
		errs = append(errs, fmt.Sprintf("%s: %v", sink.Name(), err))
		if sinkRetryable {
			anyRetryable = true
		}
	}
	return ok, anyRetryable, errs
}

// target resolves the Slack destination for a policy.
func (d *Drainer) target(policy AppPolicy) string {
	switch policy.Destination {
	case "", "dm":
		return d.Config.Slack.DM
	case "channel":
		return d.Config.Slack.Channel
	default:
		// Anything else is taken as an explicit channel or member ID.
		return policy.Destination
	}
}

// remove deletes a spooled event that has reached a terminal outcome.
func (d *Drainer) remove(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		d.logf("SPOOL ERROR: removing %s: %v", path, err)
	}
}

// park moves an undeliverable event aside. It is kept, not deleted: the human
// can inspect why, and no alert is ever destroyed by blurter's own failure.
func (d *Drainer) park(path string) {
	dir := filepath.Join(d.Paths.State, "parked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		d.logf("SPOOL ERROR: creating parked dir: %v", err)
		return
	}
	dest := filepath.Join(dir, filepath.Base(path))
	if err := os.Rename(path, dest); err != nil {
		d.logf("SPOOL ERROR: parking %s: %v", path, err)
	}
}

// audit appends an append-only record of what happened to every event.
// Suppression is as important to record as delivery: "blurter ate my
// notification" has to be answerable after the fact.
func (d *Drainer) audit(e *Event, decision Decision, outcome string, sinkErrs []string) {
	rec := map[string]any{
		"at":       d.now().UTC().Format(time.RFC3339),
		"id":       e.ID,
		"app":      e.App,
		"key":      e.Key,
		"severity": e.Severity,
		"subject":  e.Subject,
		"outcome":  outcome,
		"reason":   decision.Reason,
	}
	if len(sinkErrs) > 0 {
		rec["sinks"] = sinkErrs
	}
	data, err := json.Marshal(rec)
	if err != nil {
		d.logf("AUDIT ERROR: %v", err)
		return
	}
	path := filepath.Join(d.Paths.State, "audit.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		d.logf("AUDIT ERROR: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		d.logf("AUDIT ERROR: %v", err)
	}
}

// Run drains on a ticker until the context is cancelled.
//
// A ticker rather than filesystem watching: the poll interval bounds delivery
// latency to a few seconds, which is irrelevant for notifications, and it costs
// one directory read. Retries need a timer regardless, so watching would be an
// addition rather than a replacement.
func (d *Drainer) Run(ctx context.Context, interval time.Duration) error {
	d.logf("blurterd started (spool=%s interval=%s sinks=%d)", d.Paths.Spool, interval, len(d.Sinks))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		d.DrainOnce(ctx)
		select {
		case <-ctx.Done():
			d.logf("blurterd stopping")
			return nil
		case <-ticker.C:
		}
	}
}

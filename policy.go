// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// appState is what blurter remembers about one app between events. Kept
// per-app rather than globally so one app's alerts can never suppress or
// falsely "recover" another's — a single shared state file cannot express
// "ytt is broken but the backup job is fine".
type appState struct {
	LastStatus Severity  `json:"last_status"`
	LastDigest string    `json:"last_digest"`
	LastSentAt time.Time `json:"last_sent_at"`
}

// State is the daemon's memory. The daemon is the only writer, so this is a
// plain JSON file rather than a database: one writer means no locking, and the
// audit trail lives separately in append-only form.
type State struct {
	Apps map[string]*appState `json:"apps"`

	path string
}

// LoadState reads state.json. A missing or corrupt file yields empty state
// rather than an error: losing dedup memory costs at most one duplicate
// notification, whereas refusing to start costs every notification.
func LoadState(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state dir: %w", err)
	}
	s := &State{Apps: map[string]*appState{}, path: filepath.Join(dir, "state.json")}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading state: %w", err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		// Corrupt state is recoverable; say so and carry on.
		fmt.Fprintf(os.Stderr, "blurter: state file %s unreadable (%v); starting from empty state\n", s.path, err)
		s.Apps = map[string]*appState{}
		return s, nil
	}
	if s.Apps == nil {
		s.Apps = map[string]*appState{}
	}
	return s, nil
}

// Save writes state atomically, so a crash mid-write cannot leave state that
// parses but lies.
func (s *State) Save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("creating temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing state: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("publishing state: %w", err)
	}
	return nil
}

// Decision is what the policy engine concluded about one event, including why.
// The reason is recorded in the audit log so a suppressed alert can always be
// explained after the fact — "blurter ate my notification" must be answerable.
type Decision struct {
	Deliver bool
	Reason  string
	// Subject is the possibly-decorated subject to deliver (e.g. prefixed
	// with RECOVERED or STILL BROKEN).
	Subject string
}

// Decide applies the policy for an event without mutating state. Keeping the
// decision pure makes it directly testable and means a delivery failure cannot
// leave state claiming the alert was sent — the caller records state only after
// the sinks confirm.
func Decide(e *Event, policy AppPolicy, prev *appState, now time.Time) Decision {
	if policy.Mute {
		return Decision{Reason: "app is muted in config"}
	}
	if min := policy.MinSeverity; min != "" && e.Severity.rank() < min.rank() {
		return Decision{Reason: fmt.Sprintf("severity %s below configured minimum %s", e.Severity, min)}
	}

	// A healthy report is normally silent. It speaks up only to close out a
	// problem previously reported: an alert you never see resolved trains you
	// to ignore the channel.
	if e.Severity == SeverityOK {
		if prev == nil || prev.LastStatus != SeverityProblem {
			return Decision{Reason: "healthy, and no outstanding problem to close out"}
		}
		return Decision{
			Deliver: true,
			Reason:  "closing out a previously reported problem",
			Subject: "RECOVERED — " + e.Subject,
		}
	}

	// Repeat suppression. Without this, a problem that persists for a week
	// produces a notification every run and is promptly filtered out by the
	// human, which is worse than not alerting at all.
	if prev != nil && prev.LastDigest == e.Digest() && !prev.LastSentAt.IsZero() {
		renotify := time.Duration(0)
		if policy.RenotifyAfter != nil {
			renotify = policy.RenotifyAfter.Duration
		}
		age := now.Sub(prev.LastSentAt)
		if renotify == 0 || age < renotify {
			return Decision{Reason: fmt.Sprintf("identical %s already sent %s ago", e.Severity, age.Round(time.Minute))}
		}
		subject := e.Subject
		if e.Severity == SeverityProblem {
			subject = fmt.Sprintf("STILL BROKEN (%dd) — %s", int(age.Hours()/24), e.Subject)
		}
		return Decision{
			Deliver: true,
			Reason:  fmt.Sprintf("unchanged problem exceeded re-notify window of %s", renotify),
			Subject: subject,
		}
	}

	return Decision{Deliver: true, Reason: "new or changed event", Subject: e.Subject}
}

// Record updates state after a delivery attempt succeeded. Called only on
// success, so a network outage cannot mark an undelivered alert as sent — the
// bug that made ytt's own notifier drop alerts raised while offline.
func (s *State) Record(e *Event, now time.Time) {
	st, ok := s.Apps[e.App]
	if !ok {
		st = &appState{}
		s.Apps[e.App] = st
	}
	st.LastStatus = e.Severity
	st.LastDigest = e.Digest()
	st.LastSentAt = now
}

// RecordSuppressed updates the status without touching the send timestamp, so
// a suppressed event still transitions the app's health (letting a later
// recovery fire) without extending the re-notify window.
func (s *State) RecordSuppressed(e *Event) {
	st, ok := s.Apps[e.App]
	if !ok {
		st = &appState{}
		s.Apps[e.App] = st
	}
	st.LastStatus = e.Severity
}

// Get returns the remembered state for an app, or nil.
func (s *State) Get(app string) *appState {
	return s.Apps[app]
}

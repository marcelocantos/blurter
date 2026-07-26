// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Severity classifies an event. Only three levels, deliberately: the point of
// a notifier is to decide whether to interrupt a human, and that is close to a
// binary question. Finer gradations invite per-app taxonomies that no policy
// can act on consistently.
type Severity string

const (
	// SeverityProblem means something is wrong and the human should hear
	// about it.
	SeverityProblem Severity = "problem"
	// SeverityOK means the app is healthy. Normally silent; it exists so the
	// policy engine can close out a problem it previously reported.
	SeverityOK Severity = "ok"
	// SeverityInfo is a notable non-failure. Delivered only if the app's
	// configured minimum severity allows it.
	SeverityInfo Severity = "info"
)

func (s Severity) valid() bool {
	switch s {
	case SeverityProblem, SeverityOK, SeverityInfo:
		return true
	}
	return false
}

// rank orders severities for min-severity filtering.
func (s Severity) rank() int {
	switch s {
	case SeverityInfo:
		return 0
	case SeverityOK:
		return 1
	case SeverityProblem:
		return 2
	}
	return -1
}

// Event is one thing an app wants to tell the human. It is written to the
// spool by `blurter send` and consumed by the daemon.
//
// Events are immutable once spooled. Everything the daemon needs to decide
// delivery must be in here, because the submitting process is long gone by the
// time delivery is attempted — that asynchrony is the whole point.
type Event struct {
	ID        string    `json:"id"`
	App       string    `json:"app"`
	Severity  Severity  `json:"severity"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// Key overrides the computed dedup digest. Apps set this when the
	// human-readable text varies but the underlying problem is the same
	// (e.g. a message embedding a changing retry count).
	Key string `json:"key,omitempty"`

	// Link is an optional file path or URL the notification's click action
	// opens — typically the app's log.
	Link string `json:"link,omitempty"`
}

// Digest identifies "the same problem again" for dedup purposes. It is scoped
// per app so one app's alerts can never suppress another's, and includes
// severity so a problem and its recovery are distinct.
func (e *Event) Digest() string {
	material := e.Key
	if material == "" {
		material = e.Subject + "\x00" + e.Body
	}
	sum := sha256.Sum256([]byte(string(e.Severity) + "\x00" + e.App + "\x00" + material))
	return hex.EncodeToString(sum[:])
}

// Validate rejects events that cannot be delivered or would corrupt state.
// This runs in `blurter send`, so a malformed event is the submitter's error,
// reported synchronously, rather than a poison record the daemon retries
// forever.
func (e *Event) Validate() error {
	if e.App == "" {
		return fmt.Errorf("app is required")
	}
	// The app name indexes into config and state, and lands in file paths.
	if strings.ContainsAny(e.App, "/\\\x00") || e.App == "." || e.App == ".." {
		return fmt.Errorf("app %q must not contain path separators", e.App)
	}
	if !e.Severity.valid() {
		return fmt.Errorf("severity %q must be one of problem, ok, info", e.Severity)
	}
	if e.Subject == "" {
		return fmt.Errorf("subject is required")
	}
	return nil
}

// newEventID returns a lexically-sortable identifier: milliseconds since the
// epoch, zero-padded, plus randomness to break ties. Sorting spool filenames
// therefore yields chronological order, so the daemon delivers oldest-first
// without reading any file. Wide enough to stay 13 digits until year 5138.
func newEventID(now time.Time) (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating event id: %w", err)
	}
	return fmt.Sprintf("%013d-%s", now.UnixMilli(), hex.EncodeToString(b[:])), nil
}

// Spool writes the event into dir atomically: a temp file in the same
// directory followed by rename. A reader can therefore never observe a
// partially-written event, and a crash mid-write leaves only a stray temp file
// rather than an unparseable record the daemon would retry forever.
func (e *Event) Spool(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating spool dir: %w", err)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("encoding event: %w", err)
	}
	final := filepath.Join(dir, e.ID+".json")
	tmp, err := os.CreateTemp(dir, ".tmp-"+e.ID+"-*")
	if err != nil {
		return "", fmt.Errorf("creating temp event: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("writing event: %w", err)
	}
	// Durability before visibility: an alert that survives submission but not
	// a power cut is exactly the alert you needed.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("syncing event: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing event: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("publishing event: %w", err)
	}
	return final, nil
}

// spooledEvent is an event plus the path it came from, so the daemon can
// remove it once delivered.
type spooledEvent struct {
	Event *Event
	Path  string
}

// readSpool returns the spooled events in chronological order.
//
// Unparseable files are reported rather than silently skipped: a file the
// daemon cannot read is a bug or a corrupted write, and quietly ignoring it
// would reproduce the failure mode this whole project exists to prevent — an
// alert that vanishes without anyone being told.
func readSpool(dir string) ([]spooledEvent, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("reading spool: %w", err)}
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		// Skip in-flight temp files; they are not yet published.
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names) // IDs are time-ordered, so this is chronological

	var events []spooledEvent
	var errs []error
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s: %w", name, err))
			continue
		}
		var e Event
		if err := json.Unmarshal(data, &e); err != nil {
			errs = append(errs, fmt.Errorf("parsing %s: %w", name, err))
			continue
		}
		if err := e.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("invalid event %s: %w", name, err))
			continue
		}
		events = append(events, spooledEvent{Event: &e, Path: path})
	}
	return events, errs
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// fakeSink records deliveries and can be told to fail transiently or
// permanently, which is the distinction the retry logic turns on.
type fakeSink struct {
	name      string
	delivered []Notification
	failWith  error
	retryable bool
}

func (f *fakeSink) Name() string { return f.name }

func (f *fakeSink) Deliver(ctx context.Context, n Notification) (bool, error) {
	if f.failWith != nil {
		return f.retryable, f.failWith
	}
	f.delivered = append(f.delivered, n)
	return false, nil
}

func newTestDrainer(t *testing.T, sinks ...Sink) *Drainer {
	t.Helper()
	root := t.TempDir()
	paths := Paths{
		Config: filepath.Join(root, "config"),
		State:  filepath.Join(root, "state"),
		Spool:  filepath.Join(root, "spool"),
	}
	for _, dir := range []string{paths.Config, paths.State, paths.Spool} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	state, err := LoadState(paths.State)
	if err != nil {
		t.Fatal(err)
	}
	return &Drainer{
		Paths:  paths,
		Config: &Config{Slack: SlackConfig{DM: "UTEST"}},
		State:  state,
		Sinks:  sinks,
		Log:    io.Discard,
	}
}

func spool(t *testing.T, d *Drainer, e *Event) {
	t.Helper()
	if e.ID == "" {
		id, err := newEventID(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e.ID = id
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	if _, err := e.Spool(d.Paths.Spool); err != nil {
		t.Fatal(err)
	}
}

func spoolDepth(t *testing.T, d *Drainer) int {
	t.Helper()
	events, _ := readSpool(d.Paths.Spool)
	return len(events)
}

func TestDrainDeliversAndClearsSpool(t *testing.T) {
	sink := &fakeSink{name: "fake"}
	d := newTestDrainer(t, sink)
	spool(t, d, &Event{App: "ytt", Severity: SeverityProblem, Subject: "broken", Body: "detail"})

	delivered, deferred := d.DrainOnce(context.Background())
	if delivered != 1 || deferred != 0 {
		t.Fatalf("delivered=%d deferred=%d, want 1/0", delivered, deferred)
	}
	if len(sink.delivered) != 1 {
		t.Fatalf("sink saw %d notifications, want 1", len(sink.delivered))
	}
	if got := sink.delivered[0].Target; got != "UTEST" {
		t.Errorf("target = %q, want the configured DM", got)
	}
	if n := spoolDepth(t, d); n != 0 {
		t.Errorf("spool depth = %d after delivery, want 0", n)
	}
}

// The central property of the whole design: an alert raised while the network
// is down must survive and be delivered later, NOT be dropped and marked sent.
func TestTransientFailureRetainsEventAndDoesNotRecordState(t *testing.T) {
	sink := &fakeSink{name: "fake", failWith: fmt.Errorf("no route to host"), retryable: true}
	d := newTestDrainer(t, sink)
	spool(t, d, &Event{App: "ytt", Severity: SeverityProblem, Subject: "broken", Body: "detail"})

	delivered, deferred := d.DrainOnce(context.Background())
	if delivered != 0 || deferred != 1 {
		t.Fatalf("delivered=%d deferred=%d, want 0/1", delivered, deferred)
	}
	if n := spoolDepth(t, d); n != 1 {
		t.Fatalf("spool depth = %d, want the event retained for retry", n)
	}
	// Crucially: state must NOT say it was sent, or dedup would suppress the
	// retry and the alert would be lost permanently.
	if st := d.State.Get("ytt"); st != nil && !st.LastSentAt.IsZero() {
		t.Error("an undelivered alert must not be recorded as sent")
	}

	// Network returns: the retained event is delivered on a later drain.
	sink.failWith = nil
	d.Now = func() time.Time { return time.Now().Add(time.Hour) }
	delivered, _ = d.DrainOnce(context.Background())
	if delivered != 1 {
		t.Fatalf("delivered=%d after recovery, want 1", delivered)
	}
	if n := spoolDepth(t, d); n != 0 {
		t.Errorf("spool depth = %d, want 0 once delivered", n)
	}
}

func TestPermanentFailureParksRatherThanDeletes(t *testing.T) {
	// blurter's own failure must never destroy an alert: a human has to be
	// able to find it on disk.
	sink := &fakeSink{name: "fake", failWith: fmt.Errorf("invalid_auth"), retryable: false}
	d := newTestDrainer(t, sink)
	spool(t, d, &Event{App: "ytt", Severity: SeverityProblem, Subject: "broken"})

	d.DrainOnce(context.Background())
	if n := spoolDepth(t, d); n != 0 {
		t.Errorf("spool depth = %d, want the event moved out", n)
	}
	parked, err := os.ReadDir(filepath.Join(d.Paths.State, "parked"))
	if err != nil {
		t.Fatalf("reading parked dir: %v", err)
	}
	if len(parked) != 1 {
		t.Errorf("parked %d events, want 1", len(parked))
	}
}

func TestSuppressedEventIsAuditedWithReason(t *testing.T) {
	sink := &fakeSink{name: "fake"}
	d := newTestDrainer(t, sink)
	// An ok with no prior problem is silent.
	spool(t, d, &Event{App: "ytt", Severity: SeverityOK, Subject: "healthy"})

	d.DrainOnce(context.Background())
	if len(sink.delivered) != 0 {
		t.Fatalf("expected silence, sink saw %d", len(sink.delivered))
	}
	data, err := os.ReadFile(filepath.Join(d.Paths.State, "audit.jsonl"))
	if err != nil {
		t.Fatalf("suppression must still be audited: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["outcome"] != "suppressed" {
		t.Errorf("outcome = %v, want suppressed", rec["outcome"])
	}
	if rec["reason"] == "" || rec["reason"] == nil {
		t.Error("a suppression must record why, so it can be explained later")
	}
}

func TestDeliveryContinuesWhenOneSinkFails(t *testing.T) {
	// Any sink succeeding counts as delivered — the banner is the fallback
	// that works with no network at all.
	failing := &fakeSink{name: "slack", failWith: fmt.Errorf("no network"), retryable: true}
	working := &fakeSink{name: "banner"}
	d := newTestDrainer(t, failing, working)
	spool(t, d, &Event{App: "ytt", Severity: SeverityProblem, Subject: "broken"})

	delivered, _ := d.DrainOnce(context.Background())
	if delivered != 1 {
		t.Fatalf("delivered=%d, want 1 (banner succeeded)", delivered)
	}
	if len(working.delivered) != 1 {
		t.Error("the working sink should have received the notification")
	}
}

func TestEventsDeliverInChronologicalOrder(t *testing.T) {
	sink := &fakeSink{name: "fake"}
	d := newTestDrainer(t, sink)
	base := time.Now()
	for i, subject := range []string{"first", "second", "third"} {
		id, err := newEventID(base.Add(time.Duration(i) * time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		// Distinct bodies so dedup does not collapse them.
		spool(t, d, &Event{ID: id, App: "ytt", Severity: SeverityProblem, Subject: subject, Body: subject})
	}
	d.DrainOnce(context.Background())
	if len(sink.delivered) != 3 {
		t.Fatalf("delivered %d, want 3", len(sink.delivered))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got := sink.delivered[i].Subject; got != want {
			t.Errorf("position %d = %q, want %q", i, got, want)
		}
	}
}

func TestUnparseableSpoolFileIsReportedNotIgnored(t *testing.T) {
	d := newTestDrainer(t, &fakeSink{name: "fake"})
	if err := writeFile(filepath.Join(d.Paths.Spool, "0000000000001-zz.json"), "{ not json"); err != nil {
		t.Fatal(err)
	}
	_, errs := readSpool(d.Paths.Spool)
	if len(errs) == 0 {
		t.Error("a corrupt spool file must be reported, not silently skipped")
	}
}

func TestSpoolWriteIsAtomic(t *testing.T) {
	// Only the renamed file should be visible; temp files are hidden from
	// readers so a partial write is never delivered.
	d := newTestDrainer(t, &fakeSink{name: "fake"})
	e := &Event{ID: "0000000000009-ab", App: "ytt", Severity: SeverityProblem, Subject: "x", CreatedAt: time.Now()}
	path, err := e.Spool(d.Paths.Spool)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "0000000000009-ab.json" {
		t.Errorf("spooled as %q, want the event id", filepath.Base(path))
	}
	entries, err := os.ReadDir(d.Paths.Spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("spool holds %d files, want exactly the published event", len(entries))
	}
}

// Slack answers HTTP 200 with {"ok":false} for logical failures, so trusting
// the status code would report a phantom send.
func TestSlackSinkTreatsOkFalseAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":false,"error":"missing_scope"}`)
	}))
	defer srv.Close()

	sink := &SlackSink{Token: "xoxb-test", BaseURL: srv.URL, Client: srv.Client()}
	retryable, err := sink.Deliver(context.Background(), Notification{Target: "UTEST", Subject: "x"})
	if err == nil {
		t.Fatal("ok:false must be an error, not a phantom success")
	}
	if !strings.Contains(err.Error(), "missing_scope") {
		t.Errorf("error = %v, want Slack's own error string surfaced", err)
	}
	if retryable {
		t.Error("missing_scope is a config error; retrying it forever buries real alerts")
	}
}

func TestSlackSinkRetriesServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	sink := &SlackSink{Token: "xoxb-test", BaseURL: srv.URL, Client: srv.Client()}
	retryable, err := sink.Deliver(context.Background(), Notification{Target: "UTEST", Subject: "x"})
	if err == nil {
		t.Fatal("a 502 must be an error")
	}
	if !retryable {
		t.Error("a 5xx is transient and must be retried")
	}
}

func TestSlackSinkSendsSubjectBodyAndLink(t *testing.T) {
	var got struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		if auth := r.Header.Get("Authorization"); auth != "Bearer xoxb-test" {
			t.Errorf("Authorization = %q", auth)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	sink := &SlackSink{Token: "xoxb-test", BaseURL: srv.URL, Client: srv.Client()}
	_, err := sink.Deliver(context.Background(), Notification{
		Target: "UTEST", Subject: "subject line", Body: "the detail", Link: "/tmp/x.log",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel != "UTEST" {
		t.Errorf("channel = %q", got.Channel)
	}
	for _, want := range []string{"subject line", "the detail", "/tmp/x.log"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("text %q missing %q", got.Text, want)
		}
	}
}

func TestSlackTokenRefusesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "slack-token")
	if err := os.WriteFile(tokenPath, []byte("xoxb-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Slack: SlackConfig{TokenFile: "slack-token"}}
	_, err := cfg.SlackToken(Paths{Config: dir})
	if err == nil {
		t.Fatal("a group-readable token must be refused, not used")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error = %v, want actionable remediation", err)
	}
}

func TestSlackTokenAcceptsTightPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slack-token"), []byte("  xoxb-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Slack: SlackConfig{TokenFile: "slack-token"}}
	token, err := cfg.SlackToken(Paths{Config: dir})
	if err != nil {
		t.Fatal(err)
	}
	if token != "xoxb-secret" {
		t.Errorf("token = %q, want it trimmed", token)
	}
}

// An event that has been sitting in the spool through an outage must still get
// its full retry budget. Keying backoff off CreatedAt means an aged event never
// defers, so maxAttempts is burned in a handful of drain ticks and a real alert
// is parked instead of retried — the exact loss this project exists to prevent.
func TestAgedEventStillHonoursBackoff(t *testing.T) {
	// Matches the daemon's default drain cadence (main.go's --interval default).
	const drainInterval = 5 * time.Second
	const spoolAge = 2 * time.Hour

	sink := &fakeSink{name: "fake", failWith: fmt.Errorf("no route to host"), retryable: true}
	d := newTestDrainer(t, sink)

	clock := time.Now()
	d.Now = func() time.Time { return clock }
	spool(t, d, &Event{
		App:       "ytt",
		Severity:  SeverityProblem,
		Subject:   "broken",
		Body:      "detail",
		CreatedAt: clock.Add(-spoolAge),
	})

	for i := 0; i < maxAttempts; i++ {
		d.DrainOnce(context.Background())
		clock = clock.Add(drainInterval)
	}

	if n := spoolDepth(t, d); n != 1 {
		t.Errorf("spool depth = %d after %v of drains, want the event retained for retry",
			n, time.Duration(maxAttempts)*drainInterval)
	}
	parked, err := os.ReadDir(filepath.Join(d.Paths.State, "parked"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(parked) != 0 {
		t.Errorf("parked %d events; backoff must throttle retries by last attempt, not creation time", len(parked))
	}
}

// min_severity: problem is the documented way to quiet a chatty app. It must
// drop info chatter without clearing an outstanding problem, or the later ok
// is silent and the human never sees RECOVERED.
func TestDrainMinSeverityDoesNotClearOutstandingProblem(t *testing.T) {
	sink := &fakeSink{name: "fake"}
	d := newTestDrainer(t, sink)
	d.Config.Apps = map[string]AppPolicy{
		"chatty": {MinSeverity: SeverityProblem},
	}

	spool(t, d, &Event{App: "chatty", Severity: SeverityProblem, Subject: "broken", Body: "detail"})
	d.DrainOnce(context.Background())
	if len(sink.delivered) != 1 {
		t.Fatalf("problem must deliver, sink saw %d", len(sink.delivered))
	}

	spool(t, d, &Event{App: "chatty", Severity: SeverityInfo, Subject: "fyi"})
	d.DrainOnce(context.Background())
	if len(sink.delivered) != 1 {
		t.Fatalf("info below min_severity must not deliver, sink saw %d", len(sink.delivered))
	}

	spool(t, d, &Event{App: "chatty", Severity: SeverityOK, Subject: "healthy"})
	d.DrainOnce(context.Background())
	if len(sink.delivered) != 2 {
		t.Fatalf("ok after a below-min info must still recover, sink saw %d", len(sink.delivered))
	}
	if got := sink.delivered[1].Subject; !strings.HasPrefix(got, "RECOVERED — ") {
		t.Errorf("subject = %q, want RECOVERED prefix", got)
	}
}

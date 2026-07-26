// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func testEvent(app string, sev Severity, subject, body string) *Event {
	return &Event{ID: "0000000000001-aa", App: app, Severity: sev, Subject: subject, Body: body}
}

func week() *Duration { return &Duration{7 * 24 * time.Hour} }

func TestDecideDeliversNewProblem(t *testing.T) {
	d := Decide(testEvent("ytt", SeverityProblem, "broken", "detail"),
		AppPolicy{RenotifyAfter: week()}, nil, time.Now())
	if !d.Deliver {
		t.Fatalf("a new problem must be delivered, got %+v", d)
	}
	if d.Subject != "broken" {
		t.Errorf("subject = %q, want undecorated %q", d.Subject, "broken")
	}
}

func TestDecideSuppressesIdenticalProblemInsideWindow(t *testing.T) {
	now := time.Now()
	e := testEvent("ytt", SeverityProblem, "broken", "detail")
	prev := &appState{LastStatus: SeverityProblem, LastDigest: e.Digest(), LastSentAt: now.Add(-24 * time.Hour)}

	d := Decide(e, AppPolicy{RenotifyAfter: week()}, prev, now)
	if d.Deliver {
		t.Fatalf("an identical problem inside the window must be suppressed, got %+v", d)
	}
	if d.Reason == "" {
		t.Error("a suppression must carry a reason for the audit log")
	}
}

func TestDecideRenotifiesAfterWindow(t *testing.T) {
	now := time.Now()
	e := testEvent("ytt", SeverityProblem, "broken", "detail")
	prev := &appState{LastStatus: SeverityProblem, LastDigest: e.Digest(), LastSentAt: now.Add(-8 * 24 * time.Hour)}

	d := Decide(e, AppPolicy{RenotifyAfter: week()}, prev, now)
	if !d.Deliver {
		t.Fatalf("an unresolved problem past the window must re-alert, got %+v", d)
	}
	if want := "STILL BROKEN (8d) — broken"; d.Subject != want {
		t.Errorf("subject = %q, want %q", d.Subject, want)
	}
}

func TestDecideRenotifyZeroNeverNags(t *testing.T) {
	now := time.Now()
	e := testEvent("ytt", SeverityProblem, "broken", "detail")
	prev := &appState{LastStatus: SeverityProblem, LastDigest: e.Digest(), LastSentAt: now.Add(-900 * 24 * time.Hour)}

	d := Decide(e, AppPolicy{RenotifyAfter: &Duration{0}}, prev, now)
	if d.Deliver {
		t.Fatalf("renotify_after: 0 must disable re-notification, got %+v", d)
	}
}

func TestDecideChangedProblemAlertsEvenWhilePriorUnresolved(t *testing.T) {
	now := time.Now()
	first := testEvent("ytt", SeverityProblem, "broken", "detail one")
	prev := &appState{LastStatus: SeverityProblem, LastDigest: first.Digest(), LastSentAt: now}

	second := testEvent("ytt", SeverityProblem, "broken", "a different failure")
	d := Decide(second, AppPolicy{RenotifyAfter: week()}, prev, now)
	if !d.Deliver {
		t.Fatalf("a new problem must not be masked by an unresolved one, got %+v", d)
	}
}

func TestDecideKeyOverrideCollapsesVaryingText(t *testing.T) {
	now := time.Now()
	first := testEvent("ytt", SeverityProblem, "3 of 51 failed", "")
	first.Key = "videos-failing"
	prev := &appState{LastStatus: SeverityProblem, LastDigest: first.Digest(), LastSentAt: now}

	// Same underlying problem, different count in the text.
	second := testEvent("ytt", SeverityProblem, "4 of 52 failed", "")
	second.Key = "videos-failing"
	d := Decide(second, AppPolicy{RenotifyAfter: week()}, prev, now)
	if d.Deliver {
		t.Fatalf("--key must collapse varying text into one problem, got %+v", d)
	}
}

func TestDecideRecoveryNoticeSentOnce(t *testing.T) {
	now := time.Now()
	problem := testEvent("ytt", SeverityProblem, "broken", "detail")
	prev := &appState{LastStatus: SeverityProblem, LastDigest: problem.Digest(), LastSentAt: now}

	ok := testEvent("ytt", SeverityOK, "healthy", "")
	d := Decide(ok, AppPolicy{RenotifyAfter: week()}, prev, now)
	if !d.Deliver {
		t.Fatalf("the first ok after a problem must close it out, got %+v", d)
	}
	if want := "RECOVERED — healthy"; d.Subject != want {
		t.Errorf("subject = %q, want %q", d.Subject, want)
	}

	// After recovery, steady-state health is silent: an every-run "all good"
	// trains the human to ignore the channel.
	recovered := &appState{LastStatus: SeverityOK, LastDigest: ok.Digest(), LastSentAt: now}
	d2 := Decide(testEvent("ytt", SeverityOK, "healthy", ""), AppPolicy{RenotifyAfter: week()}, recovered, now)
	if d2.Deliver {
		t.Fatalf("steady-state health must be silent, got %+v", d2)
	}
}

func TestDecideHealthyWithNoPriorProblemIsSilent(t *testing.T) {
	d := Decide(testEvent("ytt", SeverityOK, "healthy", ""), AppPolicy{RenotifyAfter: week()}, nil, time.Now())
	if d.Deliver {
		t.Fatalf("health with nothing to recover from must be silent, got %+v", d)
	}
}

func TestDecideMinSeverityDropsInfo(t *testing.T) {
	d := Decide(testEvent("chatty", SeverityInfo, "fyi", ""),
		AppPolicy{MinSeverity: SeverityProblem, RenotifyAfter: week()}, nil, time.Now())
	if d.Deliver {
		t.Fatalf("info below min_severity must be dropped, got %+v", d)
	}
}

func TestDecideMuteSuppressesEverything(t *testing.T) {
	d := Decide(testEvent("wip", SeverityProblem, "broken", ""),
		AppPolicy{Mute: true, RenotifyAfter: week()}, nil, time.Now())
	if d.Deliver {
		t.Fatalf("a muted app must be silent even for problems, got %+v", d)
	}
}

// Per-app isolation is the property a single shared state file cannot express,
// and getting it wrong means one app's failure silences another's.
func TestDigestIsScopedPerApp(t *testing.T) {
	a := testEvent("ytt", SeverityProblem, "broken", "detail")
	b := testEvent("backup", SeverityProblem, "broken", "detail")
	if a.Digest() == b.Digest() {
		t.Error("identical text from different apps must not share a dedup digest")
	}
}

func TestDigestDistinguishesSeverity(t *testing.T) {
	problem := testEvent("ytt", SeverityProblem, "same", "text")
	ok := testEvent("ytt", SeverityOK, "same", "text")
	if problem.Digest() == ok.Digest() {
		t.Error("a problem and its recovery must not share a digest")
	}
}

func TestStateRecordOnlyOnSuccess(t *testing.T) {
	// Record moves the send timestamp; RecordSuppressed must not. Otherwise a
	// suppressed event would extend the re-notify window and an unresolved
	// problem could go quiet indefinitely.
	dir := t.TempDir()
	s, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := testEvent("ytt", SeverityProblem, "broken", "detail")
	now := time.Now()
	s.Record(e, now)
	if got := s.Get("ytt").LastSentAt; !got.Equal(now) {
		t.Fatalf("LastSentAt = %v, want %v", got, now)
	}

	later := testEvent("ytt", SeverityProblem, "broken", "detail")
	s.RecordSuppressed(later)
	if got := s.Get("ytt").LastSentAt; !got.Equal(now) {
		t.Errorf("suppression moved LastSentAt to %v; it must stay %v", got, now)
	}
}

func TestStateRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	s.Record(testEvent("ytt", SeverityProblem, "broken", ""), now)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := reloaded.Get("ytt")
	if st == nil {
		t.Fatal("state did not survive a reload")
	}
	if st.LastStatus != SeverityProblem || !st.LastSentAt.Equal(now) {
		t.Errorf("reloaded state = %+v, want status=problem sent=%v", st, now)
	}
}

func TestCorruptStateFallsBackToEmpty(t *testing.T) {
	// Losing dedup memory costs one duplicate notification; refusing to start
	// costs every notification.
	dir := t.TempDir()
	if err := writeFile(dir+"/state.json", "{ this is not json"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(dir)
	if err != nil {
		t.Fatalf("corrupt state must not be fatal: %v", err)
	}
	if len(s.Apps) != 0 {
		t.Errorf("expected empty state, got %+v", s.Apps)
	}
}

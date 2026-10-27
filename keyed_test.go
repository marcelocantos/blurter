// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"
)

func TestAlternatingKeyedProblemsSurviveRestartAndRecoverIndependently(t *testing.T) {
	sink := &fakeSink{name: "test"}
	d := newTestDrainer(t, sink)
	d.Config.Default.RenotifyAfter = week()
	for _, key := range []string{"download", "analyze", "download", "analyze"} {
		spool(t, d, &Event{App: "ytt", Key: key, Severity: SeverityProblem, Subject: key})
		d.DrainOnce(context.Background())
		// Every event runs against persisted state, like a daemon restart.
		var err error
		d.State, err = LoadState(d.Paths.State)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.delivered) != 2 {
		t.Fatalf("alternating reports delivered %d alerts, want two", len(sink.delivered))
	}
	spool(t, d, &Event{App: "ytt", Key: "download", Severity: SeverityOK, Subject: "download healthy"})
	d.DrainOnce(context.Background())
	spool(t, d, &Event{App: "ytt", Key: "analyze", Severity: SeverityProblem, Subject: "still broken"})
	d.DrainOnce(context.Background())
	if len(sink.delivered) != 3 || sink.delivered[2].Subject != "RECOVERED — download healthy" {
		t.Fatalf("unexpected deliveries after recovery: %+v", sink.delivered)
	}
	if st := d.State.ForEvent(&Event{App: "ytt", Key: "analyze"}); st.LastStatus != SeverityProblem {
		t.Fatal("download recovery cleared analyze")
	}
}

func TestLegacyKeyedDigestMigratesWithoutRenotifying(t *testing.T) {
	s, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Event{App: "ytt", Key: "analyze", Severity: SeverityProblem}
	now := time.Now()
	s.Apps["ytt"] = &appState{LastStatus: SeverityProblem, LastDigest: e.Digest(), LastSentAt: now}
	if Decide(e, AppPolicy{RenotifyAfter: week()}, s.ForEvent(e), now).Deliver {
		t.Fatal("legacy last digest was not preserved")
	}
	s.RecordSuppressed(e)
	if !s.Keys["ytt"]["analyze"].LastSentAt.Equal(now) {
		t.Fatal("migration lost send time")
	}
}

func TestLegacyKeyedProblemRecoversWhenRecoveryArrivesFirst(t *testing.T) {
	sink := &fakeSink{name: "test"}
	d := newTestDrainer(t, sink)
	problem := &Event{App: "ytt", Key: "analyze", Severity: SeverityProblem}
	d.State.Apps["ytt"] = &appState{LastStatus: SeverityProblem, LastDigest: problem.Digest(), LastSentAt: time.Now()}
	d.State.Apps["backup"] = &appState{LastStatus: SeverityProblem, LastDigest: "backup", LastSentAt: time.Now()}
	if err := d.State.Save(); err != nil {
		t.Fatal(err)
	}
	var err error
	d.State, err = LoadState(d.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"download", "analyze", "analyze"} {
		spool(t, d, &Event{App: "ytt", Key: key, Severity: SeverityOK, Subject: key + " healthy"})
		d.DrainOnce(context.Background())
	}
	if len(sink.delivered) != 1 || sink.delivered[0].Subject != "RECOVERED — analyze healthy" {
		t.Fatalf("expected only migrated analyze recovery, got %+v", sink.delivered)
	}
	d.State, err = LoadState(d.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if st := d.State.Get("backup"); st.LastStatus != SeverityProblem || st.LastDigest != "backup" {
		t.Fatalf("unrelated legacy app was changed: %+v", st)
	}
}

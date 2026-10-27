// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Delivery-policy exception: the receiver records deliveries instead of
// calling the owner's desktop or Slack. Delivery count is the property;
// using the owner endpoints would generate unwanted alerts. The real CLI,
// spool, drain, persisted state and BannerSink all run unchanged.
func TestCLIIndependentConditionsJourney(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "blurter")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	config := filepath.Join(root, "config", "blurter")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.yaml"), []byte("banner:\n  enabled: true\ndefault:\n  renotify_after: 168h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipts := filepath.Join(root, "receipts")
	notifier := filepath.Join(root, "terminal-notifier")
	if err := os.WriteFile(notifier, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BLURTER_JOURNEY_RECEIPTS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Dir(config), "XDG_STATE_HOME="+filepath.Join(root, "state"), "PATH="+root+":"+os.Getenv("PATH"), "BLURTER_JOURNEY_RECEIPTS="+receipts)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	for _, step := range []struct{ key, severity string }{
		{"download", "problem"}, {"analyze", "problem"},
		{"download", "problem"}, {"analyze", "problem"},
		{"download", "ok"}, {"analyze", "problem"}, {"download", "ok"},
	} {
		run("send", "--app", "ytt", "--key", step.key, "--severity", step.severity, "--subject", step.key+" "+step.severity)
		// Fresh process every time verifies persisted restart behavior.
		run("drain")
	}
	data, err := os.ReadFile(receipts)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], "RECOVERED — download ok") {
		t.Fatalf("expected two problems and one matching recovery, got:\n%s", data)
	}
}

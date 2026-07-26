// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpSession drives the server over the real stdio protocol and returns the
// decoded responses. Exercising the actual framing matters: a protocol mistake
// does not fail loudly, it just means the client never sees the tools.
func mcpSession(t *testing.T, paths Paths, requests ...string) []jsonrpcResponse {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var out bytes.Buffer
	s := &mcpServer{paths: paths, out: json.NewEncoder(&out), log: io.Discard}
	if err := s.serve(in); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var got []jsonrpcResponse
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("undecodable response %q: %v", line, err)
		}
		got = append(got, resp)
	}
	return got
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	p := Paths{
		Config: filepath.Join(root, "config"),
		State:  filepath.Join(root, "state"),
		Spool:  filepath.Join(root, "spool"),
	}
	for _, dir := range []string{p.Config, p.State, p.Spool} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// resultMap decodes a response's result into a map.
func resultMap(t *testing.T, resp jsonrpcResponse) map[string]any {
	t.Helper()
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// resultText extracts the text content of a tool result.
func resultText(t *testing.T, resp jsonrpcResponse) string {
	t.Helper()
	m := resultMap(t, resp)
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result has no content: %+v", m)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected content shape: %+v", content[0])
	}
	return first["text"].(string)
}

func TestMCPInitializeEchoesProtocolVersion(t *testing.T) {
	// Echoing the client's version avoids a negotiation mismatch that some
	// clients treat as fatal.
	resps := mcpSession(t, testPaths(t),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	m := resultMap(t, resps[0])
	if got := m["protocolVersion"]; got != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's version echoed", got)
	}
	caps, ok := m["capabilities"].(map[string]any)
	if !ok || caps["tools"] == nil {
		t.Errorf("server must advertise the tools capability, got %+v", m["capabilities"])
	}
}

func TestMCPNotificationsGetNoResponse(t *testing.T) {
	// Replying to a notification (no id) is a protocol violation.
	resps := mcpSession(t, testPaths(t),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want exactly 1 (the notification must not be answered)", len(resps))
	}
}

func TestMCPToolsListAdvertisesTools(t *testing.T) {
	resps := mcpSession(t, testPaths(t), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	m := resultMap(t, resps[0])
	tools, ok := m["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list returned %+v", m)
	}
	found := map[string]bool{}
	for _, tool := range tools {
		found[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"blurter_send", "blurter_status", "blurter_recent"} {
		if !found[want] {
			t.Errorf("tool %q not advertised", want)
		}
	}
}

func TestMCPSendSpoolsEvent(t *testing.T) {
	paths := testPaths(t)
	resps := mcpSession(t, paths,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blurter_send","arguments":{"subject":"build finished","body":"all green","severity":"info"}}}`)

	if m := resultMap(t, resps[0]); m["isError"] == true {
		t.Fatalf("send failed: %s", resultText(t, resps[0]))
	}
	events, errs := readSpool(paths.Spool)
	if len(errs) != 0 {
		t.Fatalf("spool errors: %v", errs)
	}
	if len(events) != 1 {
		t.Fatalf("spooled %d events, want 1", len(events))
	}
	e := events[0].Event
	if e.Subject != "build finished" || e.Body != "all green" {
		t.Errorf("event = %+v", e)
	}
	if e.App != "adhoc" {
		t.Errorf("app = %q, want the adhoc default", e.App)
	}
	// The reply must not imply delivery — the whole design is asynchronous.
	if text := resultText(t, resps[0]); !strings.Contains(text, "not delivery confirmation") {
		t.Errorf("reply should say it is not delivery confirmation, got %q", text)
	}
}

func TestMCPSendDefaultsToInfoSeverity(t *testing.T) {
	// An agent asking for a message usually means "tell me this", not
	// "something is broken"; defaulting to info keeps problem meaningful.
	paths := testPaths(t)
	mcpSession(t, paths,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blurter_send","arguments":{"subject":"just so you know"}}}`)
	events, _ := readSpool(paths.Spool)
	if len(events) != 1 {
		t.Fatalf("spooled %d, want 1", len(events))
	}
	if got := events[0].Event.Severity; got != SeverityInfo {
		t.Errorf("severity = %q, want info", got)
	}
}

func TestMCPSendRejectsMissingSubject(t *testing.T) {
	paths := testPaths(t)
	resps := mcpSession(t, paths,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blurter_send","arguments":{"body":"no subject"}}}`)
	m := resultMap(t, resps[0])
	if m["isError"] != true {
		t.Errorf("a subjectless event must be rejected, got %+v", m)
	}
	// A rejected event must not reach the spool, or the daemon would retry a
	// record it can never deliver.
	if events, _ := readSpool(paths.Spool); len(events) != 0 {
		t.Errorf("invalid event reached the spool: %+v", events)
	}
}

func TestMCPToolErrorsAreResultsNotProtocolErrors(t *testing.T) {
	// Reporting a tool failure as a JSON-RPC error reads as "the server is
	// broken" and stops some clients showing the message to the model.
	resps := mcpSession(t, testPaths(t),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`)
	if resps[0].Error != nil {
		t.Errorf("unknown tool produced a JSON-RPC error: %+v", resps[0].Error)
	}
	if m := resultMap(t, resps[0]); m["isError"] != true {
		t.Errorf("unknown tool should be an isError result, got %+v", m)
	}
}

func TestMCPUnknownMethodIsAProtocolError(t *testing.T) {
	// By contrast, an unknown *method* genuinely is a protocol error.
	resps := mcpSession(t, testPaths(t), `{"jsonrpc":"2.0","id":1,"method":"nonsense/method"}`)
	if resps[0].Error == nil {
		t.Fatal("unknown method must be a JSON-RPC error")
	}
	if resps[0].Error.Code != -32601 {
		t.Errorf("code = %d, want -32601 (method not found)", resps[0].Error.Code)
	}
}

func TestMCPStatusReportsSpoolAndSinks(t *testing.T) {
	paths := testPaths(t)
	resps := mcpSession(t, paths,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blurter_send","arguments":{"subject":"queued"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"blurter_status"}}`)
	text := resultText(t, resps[1])
	if !strings.Contains(text, "spool: 1 pending") {
		t.Errorf("status should report the pending event, got:\n%s", text)
	}
	if !strings.Contains(text, "slack:") || !strings.Contains(text, "banner:") {
		t.Errorf("status should report sink readiness, got:\n%s", text)
	}
}

func TestMCPRecentReadsAuditLog(t *testing.T) {
	paths := testPaths(t)
	audit := filepath.Join(paths.State, "audit.jsonl")
	lines := []string{
		`{"at":"2026-07-26T00:00:00Z","app":"ytt","outcome":"delivered","subject":"one"}`,
		`{"at":"2026-07-26T00:01:00Z","app":"other","outcome":"suppressed","subject":"two"}`,
		`{"at":"2026-07-26T00:02:00Z","app":"ytt","outcome":"delivered","subject":"three"}`,
	}
	if err := os.WriteFile(audit, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	all := resultText(t, mcpSession(t, paths,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blurter_recent"}}`)[0])
	if strings.Count(all, "\n") != 2 {
		t.Errorf("expected 3 entries, got:\n%s", all)
	}

	filtered := resultText(t, mcpSession(t, paths,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blurter_recent","arguments":{"app":"ytt"}}}`)[0])
	if strings.Contains(filtered, "other") {
		t.Errorf("app filter leaked another app's entries:\n%s", filtered)
	}

	limited := resultText(t, mcpSession(t, paths,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blurter_recent","arguments":{"limit":1}}}`)[0])
	if strings.Contains(limited, "\n") {
		t.Errorf("limit=1 returned more than one entry:\n%s", limited)
	}
	if !strings.Contains(limited, "three") {
		t.Errorf("limit should keep the MOST RECENT entry, got:\n%s", limited)
	}
}

func TestMCPRecentWithNoLogIsNotAnError(t *testing.T) {
	resps := mcpSession(t, testPaths(t),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blurter_recent"}}`)
	if m := resultMap(t, resps[0]); m["isError"] == true {
		t.Errorf("a missing audit log is a normal empty state, not an error")
	}
}

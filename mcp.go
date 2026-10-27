// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The MCP surface exists so an agent session can raise an ad-hoc notification
// ("tell me when this finishes") and inspect what blurter has been doing,
// without a shell round-trip.
//
// Note what it deliberately does NOT do: deliver. The tools spool events and
// read state, exactly like the CLI. Delivery stays in the daemon, with no LLM
// anywhere in that path — an agent asking for a message is a submitter like any
// other app, and gets the same durability and policy.

// mcpProtocolVersion is returned when a client does not name one.
const mcpProtocolVersion = "2024-11-05"

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// mcpServer serves MCP over stdio.
type mcpServer struct {
	paths Paths
	out   *json.Encoder
	log   io.Writer
	// now is injected for tests.
	now func() time.Time
}

func cmdMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	s := &mcpServer{paths: DefaultPaths(), out: json.NewEncoder(stdout), log: stderr}
	return s.serve(stdin)
}

func (s *mcpServer) serve(stdin io.Reader) error {
	// MCP stdio framing is newline-delimited JSON. Raise the buffer well above
	// bufio's 64 KiB default: a body pasted from a log can exceed it, and a
	// truncated line would desynchronise the stream rather than fail cleanly.
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req jsonrpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			fmt.Fprintf(s.log, "blurter mcp: unparseable message: %v\n", err)
			continue
		}
		// A request without an id is a notification: act on it, never reply.
		// Replying to one is a protocol violation that some clients treat as
		// fatal.
		if len(req.ID) == 0 {
			continue
		}
		resp := s.dispatch(&req)
		if err := s.out.Encode(resp); err != nil {
			return fmt.Errorf("writing response: %w", err)
		}
	}
	return scanner.Err()
}

func (s *mcpServer) dispatch(req *jsonrpcRequest) jsonrpcResponse {
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = s.initialize(req.Params)
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools()}
	case "tools/call":
		result, err := s.callTool(req.Params)
		if err != nil {
			// Tool failures are reported as results with isError, not as
			// JSON-RPC errors: the latter reads as "the server is broken",
			// which stops some clients from showing the message to the model.
			resp.Result = toolError(err.Error())
			break
		}
		resp.Result = result
	default:
		resp.Error = &jsonrpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func (s *mcpServer) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := p.ProtocolVersion
	if version == "" {
		version = mcpProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "blurter", "version": version_()},
	}
}

// version_ avoids shadowing the package-level version variable in a map literal.
func version_() string { return version }

func mcpTools() []map[string]any {
	return []map[string]any{
		{
			"name": "blurter_send",
			"description": "Raise a notification through blurter. The event is spooled durably and " +
				"delivered asynchronously by the blurter daemon (Slack DM and/or desktop banner), " +
				"so this returns as soon as the event is queued — it is not delivery confirmation. " +
				"Use severity 'problem' for something wrong, 'info' for a notable non-failure, and " +
				"'ok' to close out a problem previously reported for the same app.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"subject": map[string]any{
						"type":        "string",
						"description": "One-line summary. This is what shows up in the notification.",
					},
					"body": map[string]any{
						"type":        "string",
						"description": "Optional detail.",
					},
					"severity": map[string]any{
						"type":        "string",
						"enum":        []string{"problem", "ok", "info"},
						"description": "Defaults to info for ad-hoc messages.",
					},
					"app": map[string]any{
						"type": "string",
						"description": "Which application this is about. Defaults to 'adhoc'. " +
							"Dedup, re-notify windows and recovery notices are all scoped per app.",
					},
					"key": map[string]any{
						"type": "string",
						"description": "Optional dedup key, when the text varies but the underlying " +
							"problem does not.",
					},
					"link": map[string]any{
						"type":        "string",
						"description": "Optional file path or URL for the notification's click action.",
					},
				},
				"required": []string{"subject"},
			},
		},
		{
			"name": "blurter_status",
			"description": "Report blurter's health: spool depth, whether the Slack and banner sinks " +
				"are ready, and the last-known status per application.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name": "blurter_recent",
			"description": "Read recent entries from blurter's audit log: what was delivered, what was " +
				"suppressed, and why. Use this to answer 'did my notification actually go out?'",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "How many of the most recent entries to return (default 20).",
					},
					"app": map[string]any{
						"type":        "string",
						"description": "Only entries for this application.",
					},
				},
			},
		},
	}
}

func toolText(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func toolError(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	}
}

func (s *mcpServer) callTool(params json.RawMessage) (map[string]any, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid tools/call params: %w", err)
	}
	switch p.Name {
	case "blurter_send":
		return s.toolSend(p.Arguments)
	case "blurter_status":
		return s.toolStatus()
	case "blurter_recent":
		return s.toolRecent(p.Arguments)
	default:
		return nil, fmt.Errorf("unknown tool %q", p.Name)
	}
}

func (s *mcpServer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *mcpServer) toolSend(args json.RawMessage) (map[string]any, error) {
	var a struct {
		App      string `json:"app"`
		Severity string `json:"severity"`
		Subject  string `json:"subject"`
		Body     string `json:"body"`
		Key      string `json:"key"`
		Link     string `json:"link"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if a.App == "" {
		a.App = "adhoc"
	}
	if a.Severity == "" {
		// An agent asking for a message usually means "tell me this", not
		// "something is broken". Defaulting to info keeps problem meaningful.
		a.Severity = string(SeverityInfo)
	}

	now := s.clock()
	id, err := newEventID(now)
	if err != nil {
		return nil, err
	}
	e := &Event{
		ID: id, App: a.App, Severity: Severity(a.Severity), Subject: a.Subject,
		Body: a.Body, Key: a.Key, Link: a.Link, CreatedAt: now,
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if _, err := e.Spool(s.paths.Spool); err != nil {
		return nil, err
	}
	return toolText(fmt.Sprintf(
		"Spooled %s event for app %q: %s\n(queued for delivery; this is not delivery confirmation — use blurter_recent to confirm)",
		e.Severity, e.App, e.Subject)), nil
}

func (s *mcpServer) toolStatus() (map[string]any, error) {
	cfg, err := LoadConfig(s.paths)
	if err != nil {
		return nil, err
	}
	state, err := LoadState(s.paths.State)
	if err != nil {
		return nil, err
	}
	events, spoolErrs := readSpool(s.paths.Spool)

	var b strings.Builder
	fmt.Fprintf(&b, "spool: %d pending\n", len(events))
	for _, err := range spoolErrs {
		fmt.Fprintf(&b, "spool error: %v\n", err)
	}
	switch token, err := cfg.SlackToken(s.paths); {
	case err != nil:
		fmt.Fprintf(&b, "slack: unavailable (%v)\n", err)
	case token == "":
		fmt.Fprintf(&b, "slack: not configured\n")
	case cfg.Slack.DM == "" && cfg.Slack.Channel == "":
		fmt.Fprintf(&b, "slack: token present but no destination configured\n")
	default:
		fmt.Fprintf(&b, "slack: ready\n")
	}
	fmt.Fprintf(&b, "banner: %v\n", cfg.Banner.enabled())

	if parked, err := os.ReadDir(filepath.Join(s.paths.State, "parked")); err == nil && len(parked) > 0 {
		// Worth surfacing prominently: a growing parked dir means alerts are
		// being written and never delivered.
		fmt.Fprintf(&b, "parked: %d undeliverable event(s) — check configuration\n", len(parked))
	}
	if len(state.Apps) == 0 && len(state.Keys) == 0 {
		fmt.Fprintf(&b, "\nno app state yet\n")
	} else {
		fmt.Fprintf(&b, "\napps:\n")
		for app, st := range state.Apps {
			last := "never"
			if !st.LastSentAt.IsZero() {
				last = st.LastSentAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "  %s (unkeyed/legacy): status=%s last_sent=%s\n", app, st.LastStatus, last)
		}
		for app, keys := range state.Keys {
			for key, st := range keys {
				fmt.Fprintf(&b, "  %s/%s: status=%s last_sent=%s\n", app, key, st.LastStatus, st.LastSentAt.UTC().Format(time.RFC3339))
			}
		}
	}
	return toolText(b.String()), nil
}

func (s *mcpServer) toolRecent(args json.RawMessage) (map[string]any, error) {
	var a struct {
		Limit int    `json:"limit"`
		App   string `json:"app"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}

	path := filepath.Join(s.paths.State, "audit.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return toolText("no audit log yet — blurter has not processed any events"), nil
		}
		return nil, fmt.Errorf("reading audit log: %w", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var kept []string
	for _, line := range lines {
		if a.App != "" {
			var rec struct {
				App string `json:"app"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.App != a.App {
				continue
			}
		}
		kept = append(kept, line)
	}
	if len(kept) > a.Limit {
		kept = kept[len(kept)-a.Limit:]
	}
	if len(kept) == 0 {
		return toolText("no matching audit entries"), nil
	}
	return toolText(strings.Join(kept, "\n")), nil
}

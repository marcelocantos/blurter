// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Sink is one delivery channel. Delivery is attempted against every configured
// sink; the event counts as delivered if any succeeds.
//
// A sink distinguishes two failure kinds through the retryable return: a
// transient failure (no network, Slack 5xx) should be retried, while a
// permanent one (bad token, unknown user) should not, because retrying it
// forever fills the spool and buries real alerts.
type Sink interface {
	Name() string
	Deliver(ctx context.Context, n Notification) (retryable bool, err error)
}

// Notification is a decided event, ready to deliver.
type Notification struct {
	App      string
	Severity Severity
	Subject  string
	Body     string
	Link     string
	// Target is the resolved Slack destination (member or channel ID).
	Target string
}

// SlackSink delivers via chat.postMessage.
//
// Deliberately a plain HTTPS call with no LLM anywhere in the path: this has to
// work precisely when other things are broken. (Measured 2026-07-26, a headless
// `claude` had its Slack connector available in 1 of 6 runs, which is
// disqualifying for a monitoring path.)
type SlackSink struct {
	Token  string
	Client *http.Client
	// BaseURL allows tests to point at a local server. Empty means Slack.
	BaseURL string
}

func (s *SlackSink) Name() string { return "slack" }

func (s *SlackSink) Deliver(ctx context.Context, n Notification) (bool, error) {
	if s.Token == "" {
		return false, fmt.Errorf("no slack token configured")
	}
	if n.Target == "" {
		return false, fmt.Errorf("no slack destination configured (set slack.dm or slack.channel)")
	}

	text := n.Subject
	if n.Body != "" {
		text += "\n" + n.Body
	}
	if n.Link != "" {
		text += "\n" + n.Link
	}
	payload, err := json.Marshal(map[string]string{"channel": n.Target, "text": text})
	if err != nil {
		return false, fmt.Errorf("encoding slack payload: %w", err)
	}

	base := s.BaseURL
	if base == "" {
		base = "https://slack.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/chat.postMessage", bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Errorf("building slack request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport failure: almost always no network. Retry.
		return true, fmt.Errorf("slack request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return true, fmt.Errorf("slack returned %s", resp.Status)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return true, fmt.Errorf("slack rate-limited the request")
	}

	// chat.postMessage answers HTTP 200 with {"ok":false,"error":"..."} for
	// logical failures, so the status code alone is not success. Trusting it
	// would report a phantom send — the same silent-success failure this
	// project exists to eliminate.
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return true, fmt.Errorf("slack returned unparseable response (%s): %w", resp.Status, err)
	}
	if body.OK {
		return false, nil
	}
	// Config errors will never succeed on retry; surface Slack's own error
	// string, which names the exact misconfiguration.
	switch body.Error {
	case "invalid_auth", "not_authed", "token_revoked", "account_inactive",
		"missing_scope", "channel_not_found", "is_archived", "no_permission":
		return false, fmt.Errorf("slack rejected the message: %s", body.Error)
	}
	return true, fmt.Errorf("slack error: %s", body.Error)
}

// BannerSink shows a desktop notification. It needs no credentials and no
// network, so it is the sink of last resort — and the only one that still
// works when the machine is offline.
type BannerSink struct {
	// LookPath and Run are injected so tests can observe invocations without
	// a real desktop session.
	LookPath func(string) (string, error)
	Run      func(name string, args ...string) error
}

func (b *BannerSink) Name() string { return "banner" }

func (b *BannerSink) lookPath(file string) (string, error) {
	if b.LookPath != nil {
		return b.LookPath(file)
	}
	return exec.LookPath(file)
}

func (b *BannerSink) run(name string, args ...string) error {
	if b.Run != nil {
		return b.Run(name, args...)
	}
	return exec.Command(name, args...).Run()
}

func (b *BannerSink) Deliver(ctx context.Context, n Notification) (bool, error) {
	firstLine := n.Body
	if i := strings.IndexByte(firstLine, '\n'); i >= 0 {
		firstLine = firstLine[:i]
	}
	if firstLine == "" {
		firstLine = n.Subject
	}

	// terminal-notifier is preferred over osascript because it posts from its
	// own app bundle, so -execute gives the notification a working click
	// action. An osascript notification is posted on behalf of Script Editor
	// with no handler and no surviving process, so macOS still renders a
	// "Show" button but clicking it does nothing — inert by construction.
	if path, err := b.lookPath("terminal-notifier"); err == nil {
		args := []string{"-title", "blurter: " + n.App, "-subtitle", n.Subject, "-message", firstLine}
		if n.Link != "" {
			args = append(args, "-execute", "open "+shellQuote(n.Link))
		}
		if err := b.run(path, args...); err != nil {
			return true, fmt.Errorf("terminal-notifier failed: %w", err)
		}
		return false, nil
	}

	if path, err := b.lookPath("osascript"); err == nil {
		script := fmt.Sprintf(
			"display notification %s with title %s subtitle %s",
			appleScriptString(firstLine), appleScriptString("blurter: "+n.App), appleScriptString(n.Subject),
		)
		if err := b.run(path, "-e", script); err != nil {
			return true, fmt.Errorf("osascript failed: %w", err)
		}
		return false, nil
	}

	return false, fmt.Errorf("no desktop notification tool found (install terminal-notifier)")
}

// shellQuote wraps s for safe inclusion in the single -execute shell string
// terminal-notifier runs.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleScriptString renders a Go string as an AppleScript literal. AppleScript
// has no escape for a literal backslash inside a quoted string beyond doubling
// it, and an unescaped quote would end the literal and turn user text into
// code.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	// Newlines cannot appear literally in an AppleScript string.
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return `"` + s + `"`
}

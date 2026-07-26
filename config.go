// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Paths locates blurter's three directories. They are separated on purpose:
// config is hand-edited and backed up, state is machine-local and disposable,
// and the spool is a queue. Conflating them is how a state file ends up in a
// git-tracked tree or a credential ends up in a backup.
type Paths struct {
	Config string // hand-edited: config.yaml, slack-token
	State  string // machine-local: state.json, audit.jsonl
	Spool  string // the queue
}

// DefaultPaths resolves the XDG locations. Config and state are deliberately
// NOT derived from the executable's location: an installed binary lives in a
// package-managed prefix that is replaced wholesale on upgrade, so config
// resolved relative to it silently disappears. That exact bug cost the ytt
// scheduler fifteen days of silent no-ops.
func DefaultPaths() Paths {
	return Paths{
		Config: filepath.Join(xdgDir("XDG_CONFIG_HOME", ".config"), "blurter"),
		State:  filepath.Join(xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state")), "blurter"),
		Spool:  filepath.Join(xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state")), "blurter", "spool"),
	}
}

func xdgDir(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Nothing sensible to fall back to; a relative path at least fails
		// loudly and locally rather than writing somewhere surprising.
		return fallback
	}
	return filepath.Join(home, fallback)
}

// Duration wraps time.Duration so YAML can carry "168h" instead of a raw
// nanosecond count.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// AppPolicy is the per-app delivery policy. Zero values mean "inherit the
// default", so an app section can override one field without restating the
// rest.
type AppPolicy struct {
	// Destination overrides the Slack target: "dm" or a channel ID.
	Destination string `yaml:"destination"`
	// RenotifyAfter is how long an unchanged, unresolved problem stays quiet
	// before alerting again. Zero disables re-notification.
	RenotifyAfter *Duration `yaml:"renotify_after"`
	// MinSeverity drops anything less severe than this.
	MinSeverity Severity `yaml:"min_severity"`
	// Mute suppresses the app entirely — useful for a noisy app you are
	// actively working on, without editing the app itself.
	Mute bool `yaml:"mute"`
}

// Config is blurter's whole configuration.
type Config struct {
	Slack  SlackConfig          `yaml:"slack"`
	Banner BannerConfig         `yaml:"banner"`
	Apps   map[string]AppPolicy `yaml:"apps"`

	// Default applies to any app without its own section, so a new app gets
	// sensible delivery without a config change. An unknown app that cannot
	// notify is worse than one that notifies with defaults.
	Default AppPolicy `yaml:"default"`
}

// SlackConfig configures the Slack sink.
type SlackConfig struct {
	// TokenFile holds a bot token (xoxb-...). Relative paths resolve against
	// the config dir. The token is never inlined in this file so that config
	// can be shared or version-controlled while the credential is not.
	TokenFile string `yaml:"token_file"`
	// DM is the member ID to direct-message (e.g. U01234567).
	DM string `yaml:"dm"`
	// Channel is used when an app's destination is "channel" and no explicit
	// channel ID is given.
	Channel string `yaml:"channel"`
}

// BannerConfig configures the local desktop-notification sink.
type BannerConfig struct {
	// Enabled defaults to true; a local banner needs no credentials and is
	// the only sink that works with no network at all.
	Enabled *bool `yaml:"enabled"`
}

func (b BannerConfig) enabled() bool {
	return b.Enabled == nil || *b.Enabled
}

// defaultRenotify is the fallback re-notify window: long enough that an
// unresolved problem does not nag daily, short enough that it is not forgotten.
const defaultRenotify = 7 * 24 * time.Hour

// PolicyFor merges an app's policy over the defaults.
func (c *Config) PolicyFor(app string) AppPolicy {
	merged := c.Default
	if merged.MinSeverity == "" {
		merged.MinSeverity = SeverityInfo
	}
	if merged.RenotifyAfter == nil {
		merged.RenotifyAfter = &Duration{defaultRenotify}
	}
	if merged.Destination == "" {
		merged.Destination = "dm"
	}

	specific, ok := c.Apps[app]
	if !ok {
		return merged
	}
	if specific.Destination != "" {
		merged.Destination = specific.Destination
	}
	if specific.RenotifyAfter != nil {
		merged.RenotifyAfter = specific.RenotifyAfter
	}
	if specific.MinSeverity != "" {
		merged.MinSeverity = specific.MinSeverity
	}
	// Mute is a plain bool: an app section can only turn muting on, which is
	// the only direction that makes sense (the default is unmuted).
	if specific.Mute {
		merged.Mute = true
	}
	return merged
}

// LoadConfig reads config.yaml. A missing file is not an error: blurter with no
// configuration still delivers local banners, which is more useful than
// refusing to start and thereby dropping every event.
func LoadConfig(paths Paths) (*Config, error) {
	cfg := &Config{}
	path := filepath.Join(paths.Config, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, sev := range []Severity{cfg.Default.MinSeverity} {
		if sev != "" && !sev.valid() {
			return nil, fmt.Errorf("%s: default.min_severity %q is not a valid severity", path, sev)
		}
	}
	for app, policy := range cfg.Apps {
		if policy.MinSeverity != "" && !policy.MinSeverity.valid() {
			return nil, fmt.Errorf("%s: apps.%s.min_severity %q is not a valid severity", path, app, policy.MinSeverity)
		}
	}
	return cfg, nil
}

// SlackToken reads the bot token, refusing a file that others can read.
//
// This is the one moment a leaked credential can be caught, and a loud refusal
// beats a quiet compromise. Probing the mode has to be done on the FileInfo
// rather than by shelling out to stat, whose flags differ between GNU and BSD.
func (c *Config) SlackToken(paths Paths) (string, error) {
	if c.Slack.TokenFile == "" {
		return "", nil
	}
	path := c.Slack.TokenFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(paths.Config, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("slack token file %s does not exist", path)
		}
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("refusing to read %s: mode %#o grants group/other access; run: chmod 600 %s", path, mode, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("slack token file %s is empty", path)
	}
	return token, nil
}

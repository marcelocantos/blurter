// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"
)

// serviceLabel is the launchd label and plist basename.
const serviceLabel = "com.marcelocantos.blurterd"

// launchdPlist is generated rather than committed. A checked-in plist would
// have to hardcode one user's home directory and binary location, which is both
// wrong for everyone else and a personal detail in a public repo. Generating it
// at install time resolves the real paths on the machine that will run it.
//
// KeepAlive restarts the daemon if it dies; RunAtLoad starts it on login. The
// spool makes both safe: events accumulate while the daemon is down and are
// delivered when it returns, so a restart loses nothing.
const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.Binary}}</string>
        <string>daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>{{.Path}}</string>
    </dict>
</dict>
</plist>
`

func cmdService(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: blurter service install|uninstall|path")
	}
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the generated plist instead of writing it")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service management is implemented for launchd (macOS) only; on Linux run `blurter daemon` under systemd --user")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locating home directory: %w", err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")

	switch args[0] {
	case "path":
		fmt.Fprintln(stdout, plistPath)
		return nil
	case "uninstall":
		// Best-effort unload before removing, so the running daemon stops.
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", plistPath, err)
		}
		fmt.Fprintf(stdout, "removed %s\n", plistPath)
		return nil
	case "install":
		// proceed below
	default:
		return fmt.Errorf("unknown service subcommand %q (install|uninstall|path)", args[0])
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the blurter binary: %w", err)
	}
	// Resolve symlinks so the plist names a stable path. A Homebrew symlink in
	// bin/ retargets on upgrade, which is fine, but resolving makes the
	// recorded path unambiguous when diagnosing which build actually ran.
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}

	logDir := filepath.Join(home, ".local", "var", "log", "blurter")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}

	data := struct{ Label, Binary, LogPath, Path string }{
		Label:   serviceLabel,
		Binary:  binary,
		LogPath: filepath.Join(logDir, "blurterd.log"),
		// launchd starts with a deliberately minimal PATH. The banner sink
		// shells out to terminal-notifier, so the daemon needs the Homebrew
		// bin directory on PATH or the sink silently disappears — the class of
		// bug that motivated this project.
		Path: "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}

	tmpl, err := template.New("plist").Parse(launchdPlist)
	if err != nil {
		return fmt.Errorf("parsing plist template: %w", err)
	}
	if *dryRun {
		return tmpl.Execute(stdout, data)
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("creating LaunchAgents dir: %w", err)
	}
	f, err := os.OpenFile(plistPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("writing %s: %w", plistPath, err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("rendering plist: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", plistPath, err)
	}

	// Reload so an existing service picks up a changed binary path.
	_ = exec.Command("launchctl", "unload", plistPath).Run()
	if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load failed: %v: %s", err, out)
	}
	fmt.Fprintf(stdout, "installed and loaded %s\n", plistPath)
	fmt.Fprintf(stdout, "logs: %s\n", data.LogPath)
	return nil
}

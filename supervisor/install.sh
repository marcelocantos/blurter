#!/bin/bash
# Transfer daemon ownership from launchd to Supervisor.
set -euo pipefail

repo="$(CDPATH='' cd "$(dirname "$0")/.." && pwd -P)"
conf_dir="${SUPERVISOR_CONF_DIR:-/opt/homebrew/etc/supervisor.d}"
dest="$conf_dir/blurter.ini"

if [[ "${SUPERVISOR_SKIP_CTL:-}" != 1 ]]; then
	if ! command -v supervisorctl >/dev/null 2>&1; then
		echo "blurter: supervisorctl is required to activate $dest" >&2
		exit 1
	fi
	# Check control access before stopping the currently running daemon.
	supervisorctl pid >/dev/null
	"$repo/supervisor/run-blurter.sh" --print-bin >/dev/null
fi

mkdir -p "$conf_dir" "$HOME/.local/var/log"
tmp="$(mktemp "$conf_dir/.blurter.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
sed "s|@REPO@|$repo|g" "$repo/supervisor/blurter.ini" >"$tmp"
chmod 644 "$tmp"
mv "$tmp" "$dest"
echo "rendered $dest"

if [[ "${SUPERVISOR_SKIP_CTL:-}" == 1 ]]; then
	exit 0
fi

# Both historical launchd installers may have been used. Stop them before
# Supervisor starts another drain loop over the shared spool.
if command -v brew >/dev/null 2>&1; then
	brew services stop blurter >/dev/null 2>&1 || true
fi
if command -v launchctl >/dev/null 2>&1; then
	launchctl bootout "gui/$(id -u)/homebrew.mxcl.blurter" 2>/dev/null || true
	launchctl bootout "gui/$(id -u)/com.marcelocantos.blurterd" 2>/dev/null || true
	for label in homebrew.mxcl.blurter com.marcelocantos.blurterd; do
		if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
			echo "blurter: launchd job $label is still loaded; refusing a second daemon" >&2
			exit 1
		fi
	done
fi
rm -f "$HOME/Library/LaunchAgents/homebrew.mxcl.blurter.plist" \
	"$HOME/Library/LaunchAgents/com.marcelocantos.blurterd.plist"

supervisorctl reread
supervisorctl update blurter
supervisorctl restart blurter
supervisorctl status blurter

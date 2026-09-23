#!/bin/bash
# Run the installed release so every submitter uses the same daemon version.
set -euo pipefail

export PATH="/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:${HOME}/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

if [[ -n "${BLURTER_BIN:-}" ]]; then
	bin="$BLURTER_BIN"
else
	if ! command -v brew >/dev/null 2>&1; then
		echo "blurter: Homebrew is required; set BLURTER_BIN to override" >&2
		exit 1
	fi
	bin="$(brew --prefix)/opt/blurter/bin/blurter"
fi

if [[ ! -x "$bin" ]]; then
	echo "blurter: binary is not executable: $bin" >&2
	exit 1
fi
if [[ "${1:-}" == "--print-bin" ]]; then
	printf '%s\n' "$bin"
	exit 0
fi

echo "blurter: running $bin daemon" >&2
exec "$bin" daemon

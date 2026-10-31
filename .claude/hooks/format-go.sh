#!/usr/bin/env bash
# PostToolUse: format a Go file right after the agent edits it, so formatting
# never shows up as an eval failure or a noisy diff.
set -euo pipefail

file=$(jq -r '.tool_input.file_path // empty')
[[ "$file" == *.go && -f "$file" ]] || exit 0

cd "${CLAUDE_PROJECT_DIR:-.}"
mise exec -- golangci-lint fmt "$file" >/dev/null 2>&1 || mise exec -- gofmt -w "$file" || true

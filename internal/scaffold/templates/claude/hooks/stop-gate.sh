#!/usr/bin/env bash
# Stop: refuse to end the turn while Go changes fail the quick eval
# (build, vet, short tests). Output is fed back to the agent to fix.
set -uo pipefail

input=$(cat)
# Already continuing because of this hook once: don't loop forever.
[[ $(jq -r '.stop_hook_active // false' <<<"$input") == "true" ]] && exit 0

cd "${CLAUDE_PROJECT_DIR:-.}"
# Only gate when Go sources or module files have uncommitted changes.
git status --porcelain --untracked-files=all -- '*.go' go.mod go.sum | grep -q . || exit 0

if ! out=$(mise exec -- {{.Ace}} eval -quick 2>&1); then
  {
    echo "Quick eval failed on your uncommitted Go changes. Fix these before finishing"
    echo "(full details: .eval/report.json):"
    echo
    tail -n 80 <<<"$out"
  } >&2
  exit 2
fi
exit 0

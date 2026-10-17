#!/bin/bash
# SessionStart hook (matcher: compact): after a compaction, print the repo
# state and the newest part of the roadmap-progress memory, so the fresh
# context starts from what's on disk and not only from the summary.
# Claude Code moves hook output over 10,000 characters into a file, so the
# memory is cut to its last ~7,000 bytes (the newest entries are at the bottom).
cd "${CLAUDE_PROJECT_DIR:-.}" || exit
mem="$HOME/.claude/projects/$(sed 's/[^A-Za-z0-9]/-/g' <<<"$PWD")/memory/roadmap-progress.md"

echo "## Repo state after compaction"
git status --short --branch | head -30
git log --oneline -5

if [ -f "$mem" ]; then
  echo
  echo "## Newest part of $mem"
  tail -c 7000 "$mem" | tail -n +2
fi

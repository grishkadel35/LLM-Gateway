#!/bin/bash
# Status line: the main session is the director; show its model and effort.
input=$(cat)
model=$(jq -r '.model.display_name // .model.id // "?"' <<<"$input")
effort=$(jq -r '.effort.level // .effort // empty' <<<"$input" 2>/dev/null)
effort="${effort:-${CLAUDE_CODE_EFFORT_LEVEL:-default}}"
echo "director · $model · $effort"

#!/bin/bash
# SubagentStart/SubagentStop hook: show which agent started or finished and
# the model and effort it runs on, read from its .claude/agents/<name>.md.
# Usage: agent-label.sh start|stop   (hook payload JSON on stdin)
input=$(cat)
agent=$(jq -r '.agent_type // "agent"' <<<"$input")
file="${CLAUDE_PROJECT_DIR:-.}/.claude/agents/$agent.md"

if [ -f "$file" ]; then
  model=$(sed -n 's/^model: *//p' "$file" | head -1)
  effort=$(sed -n 's/^effort: *//p' "$file" | head -1)
else
  model="session model"
  effort="session effort"
fi

case "$model" in
  claude-opus-5-5) model="Opus 5.5" ;;
  claude-sonnet-5-5) model="Sonnet 5.5" ;;
  claude-haiku-5-5) model="Haiku 5.5" ;;
esac

if [ "$1" = stop ]; then verb="✓ done"; else verb="▶ started"; fi
jq -n --arg m "$verb  $agent · $model · $effort" '{systemMessage: $m}'

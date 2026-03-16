#!/usr/bin/env sh

# Usage:
# 1) Fill required values below, OR export them before sourcing this file.
# 2) Run: source ./env.sh
#
# This file is intentionally kept in the repo as a reminder to avoid
# forgetting required environment variables.

# Required for Feishu bot.
export FEISHU_APP_ID="${FEISHU_APP_ID:-}"
export FEISHU_APP_SECRET="${FEISHU_APP_SECRET:-}"

# Required for Kimi model access.
export KIMI_API_KEY="${KIMI_API_KEY:-}"

# Optional settings.
export KIMI_BASE_URL="${KIMI_BASE_URL:-https://api.moonshot.cn}"
export KIMI_MODEL="${KIMI_MODEL:-moonshot-v1-8k}"
export AGENT_SKILL_URL="${AGENT_SKILL_URL:-https://github.com/pingcap/agent-rules/blob/main/skills/tidb-query-tuning/SKILL.md}"

missing_required=""

if [ -z "$FEISHU_APP_ID" ]; then
  missing_required="$missing_required FEISHU_APP_ID"
fi
if [ -z "$FEISHU_APP_SECRET" ]; then
  missing_required="$missing_required FEISHU_APP_SECRET"
fi
if [ -z "$KIMI_API_KEY" ]; then
  missing_required="$missing_required KIMI_API_KEY"
fi

if [ -n "$missing_required" ]; then
  echo "[env.sh] Missing required env vars:$missing_required"
  echo "[env.sh] Please set them before running: go run ."
else
  echo "[env.sh] Required env vars are set."
fi

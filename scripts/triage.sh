#!/usr/bin/env bash
# triage.sh — Automated email triage via Claude Code
#
# Reads new messages from the msgvault archive and classifies them:
# - Deterministic rules: auto-archive promotions, social, known automated senders
# - AI classification: keep human emails in INBOX, archive the rest
#
# Usage: triage.sh <MSGVAULT_HOME>
# All output goes to stdout (captured by journald when run as a systemd service).

set -euo pipefail

MSGVAULT_HOME="${1:?Usage: triage.sh <MSGVAULT_HOME>}"
export MSGVAULT_HOME

TRIAGE_DIR="$MSGVAULT_HOME/triage"
LAST_RUN_FILE="$TRIAGE_DIR/last_run"
RULES_FILE="$TRIAGE_DIR/rules.md"

# msgvault is reached through the mcpproxy gateway already present in Claude
# Code's managed MCP config (/etc/claude-code/managed-mcp.json). We do NOT pass
# a per-run --mcp-config: Claude Code rejects dynamic --mcp-config whenever an
# enterprise/managed config is present ("You cannot dynamically configure MCP
# servers when an enterprise MCP config is present"). Map this run's archive to
# its mcpproxy upstream server name.
case "$MSGVAULT_HOME" in
    *msgvault-work*) PROXY_SERVER="msgvault-work" ;;
    *)               PROXY_SERVER="msgvault-personal" ;;
esac

# Validate triage directory and rules file exist
if [[ ! -d "$TRIAGE_DIR" ]]; then
    echo "ERROR: triage directory not found: $TRIAGE_DIR"
    echo "Create it with: mkdir -p $TRIAGE_DIR && cp scripts/triage-rules.md $TRIAGE_DIR/rules.md"
    exit 1
fi

if [[ ! -f "$RULES_FILE" ]]; then
    echo "ERROR: required file not found: $RULES_FILE"
    exit 1
fi

# Read last triage timestamp (default: 24 hours ago)
if [[ -f "$LAST_RUN_FILE" ]]; then
    LAST_RUN=$(cat "$LAST_RUN_FILE")
else
    LAST_RUN=$(date -u -d '24 hours ago' '+%Y-%m-%dT%H:%M:%SZ')
    echo "No last_run file found, defaulting to: $LAST_RUN"
fi

NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
# For Gmail search: use date portion only. after: is inclusive of that day.
AFTER_DATE="${LAST_RUN%T*}"
echo "Triage run: $LAST_RUN → $NOW (search after:$AFTER_DATE label:inbox)"
echo "MSGVAULT_HOME: $MSGVAULT_HOME"
echo "mcpproxy upstream: $PROXY_SERVER"

PROMPT="Triage all unread/unprocessed INBOX messages since $AFTER_DATE.

All msgvault tools are reached through the mcpproxy gateway. The msgvault
instance for THIS run is the upstream server named \"$PROXY_SERVER\".

Tool access (do this exactly):
- FIRST call mcp__mcpproxy__retrieve_tools with query \"$PROXY_SERVER messages labels\"
  to load the exact msgvault tool names and their input schemas for the
  \"$PROXY_SERVER\" server.
- Read operations (search_messages, get_message, list_messages, list_gmail_labels):
  call mcp__mcpproxy__call_tool_read with name=\"$PROXY_SERVER:<tool>\" and the args
  from that tool's schema.
- Label changes (modify_labels): call mcp__mcpproxy__call_tool_write with
  name=\"$PROXY_SERVER:modify_labels\". Always pass intent_reason and
  intent_data_sensitivity=\"private\".

Steps:
1. search_messages to find messages still in INBOX (query: \"label:inbox after:$AFTER_DATE\"). Page through all results.
2. For each message, check the deterministic rules first (see system prompt). If a rule matches, apply it immediately via modify_labels.
3. For messages not caught by deterministic rules, read the subject + from + snippet and classify using the AI rules.
4. Apply label changes via modify_labels (remove the INBOX label to archive).
5. Output a JSON array summarizing all actions taken.

Important:
- Only process messages that are in INBOX (the query already filters for this).
- When in doubt, keep the message in INBOX (conservative).
- Process ALL matching messages, paging through results as needed."

echo "Running claude triage..."

set +e
CLAUDE_OUT=$(claude -p \
    --model sonnet \
    --max-turns 50 \
    --allowedTools "mcp__mcpproxy__retrieve_tools,mcp__mcpproxy__call_tool_read,mcp__mcpproxy__call_tool_write,mcp__mcpproxy__read_cache" \
    --permission-mode dontAsk \
    --output-format json \
    --no-session-persistence \
    --append-system-prompt-file "$RULES_FILE" \
    "$PROMPT")
EXIT_CODE=$?
set -e

# Emit Claude's JSON result to journald for debugging.
printf '%s\n' "$CLAUDE_OUT"

if [[ $EXIT_CODE -ne 0 ]]; then
    echo "ERROR: claude exited with code $EXIT_CODE — NOT advancing last_run"
    exit $EXIT_CODE
fi

# Guard against false success. claude -p exits 0 even when it did NO work —
# e.g. it was blocked by a permission denial (call_tool_destructive not allowed)
# or hit an internal error and just *described* the problem. Advancing last_run
# in that case silently skips the whole window forever. Only advance when the
# result JSON shows a clean run: not is_error AND no permission_denials.
RUN_STATUS=$(printf '%s' "$CLAUDE_OUT" | python3 -c '
import sys, json
try:
    line = [l for l in sys.stdin.read().splitlines() if l.strip()][-1]
    d = json.loads(line)
except Exception:
    print("PARSE_FAIL"); sys.exit(0)
if d.get("is_error"):
    print("ERROR")
elif (d.get("permission_denials") or []):
    print("BLOCKED:%d" % len(d["permission_denials"]))
else:
    print("OK")
' 2>/dev/null)

if [[ "$RUN_STATUS" != "OK" ]]; then
    echo "ERROR: triage did not complete cleanly (status=$RUN_STATUS) — NOT advancing last_run"
    exit 1
fi

echo "$NOW" > "$LAST_RUN_FILE"
echo "Triage complete. Updated last_run to $NOW"

# Re-sync from Gmail so the local archive reflects label changes made by triage
MSGVAULT_BIN="$(dirname "$0")/../msgvault"
if [[ -x "$MSGVAULT_BIN" ]]; then
    echo "Syncing local archive from Gmail..."
    "$MSGVAULT_BIN" sync --home "$MSGVAULT_HOME" 2>&1 || echo "WARNING: post-triage sync failed (non-fatal)"
fi

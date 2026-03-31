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
MCP_CONFIG="$TRIAGE_DIR/mcp.json"
RULES_FILE="$TRIAGE_DIR/rules.md"

# Validate triage directory and config exist
if [[ ! -d "$TRIAGE_DIR" ]]; then
    echo "ERROR: triage directory not found: $TRIAGE_DIR"
    echo "Create it with: mkdir -p $TRIAGE_DIR && cp scripts/triage-rules.md $TRIAGE_DIR/rules.md"
    exit 1
fi

for f in "$MCP_CONFIG" "$RULES_FILE"; do
    if [[ ! -f "$f" ]]; then
        echo "ERROR: required file not found: $f"
        exit 1
    fi
done

# Read last triage timestamp (default: 24 hours ago)
if [[ -f "$LAST_RUN_FILE" ]]; then
    LAST_RUN=$(cat "$LAST_RUN_FILE")
else
    LAST_RUN=$(date -u -d '24 hours ago' '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null \
            || date -u -v-24H '+%Y-%m-%dT%H:%M:%SZ')
    echo "No last_run file found, defaulting to: $LAST_RUN"
fi

NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
echo "Triage run: $LAST_RUN → $NOW"
echo "MSGVAULT_HOME: $MSGVAULT_HOME"

PROMPT="Triage all messages received between $LAST_RUN and $NOW.

Steps:
1. Use search_messages to find messages received in that window (query: \"after:${LAST_RUN%T*} before:${NOW%T*}\"). Page through all results.
2. For each message, check the deterministic rules first (see system prompt). If a rule matches, apply it immediately via modify_labels.
3. For messages not caught by deterministic rules, read the subject + from + snippet and classify using the AI rules.
4. Apply label changes via modify_labels (remove INBOX label to archive, add labels as needed).
5. Output a JSON array summarizing all actions taken.

Important:
- Never modify messages that are already archived (not in INBOX).
- When in doubt, keep the message in INBOX (conservative).
- Process ALL messages in the time window, paging through results as needed."

echo "Running claude triage..."

claude -p \
    --model sonnet \
    --max-turns 30 \
    --mcp-config "$MCP_CONFIG" \
    --allowedTools "mcp__msgvault__search_messages,mcp__msgvault__get_message,mcp__msgvault__list_messages,mcp__msgvault__list_gmail_labels,mcp__msgvault__modify_labels" \
    --permission-mode bypassPermissions \
    --output-format json \
    --no-session-persistence \
    --append-system-prompt-file "$RULES_FILE" \
    "$PROMPT"

EXIT_CODE=$?

if [[ $EXIT_CODE -eq 0 ]]; then
    echo "$NOW" > "$LAST_RUN_FILE"
    echo "Triage complete. Updated last_run to $NOW"
else
    echo "ERROR: claude exited with code $EXIT_CODE"
    exit $EXIT_CODE
fi

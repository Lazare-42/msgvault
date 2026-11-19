#!/usr/bin/env bash
# wealthfolio-order-sync.sh — Extract executed trade recaps from email using
# Claude + msgvault MCP, then write JSON envelopes for Wealthfolio ingestion.

set -euo pipefail

MSGVAULT_HOME="${1:?Usage: wealthfolio-order-sync.sh <MSGVAULT_HOME>}"
SYNC_DIR="$MSGVAULT_HOME/wealthfolio-orders"
LAST_RUN_FILE="$SYNC_DIR/last_run"
STATE_FILE="$SYNC_DIR/state.json"
CONFIG_FILE="$SYNC_DIR/config.json"
RULES_FILE="$SYNC_DIR/rules.md"
# WEALTHFOLIO_DRY_RUN=1 prints the new envelopes and stops before writing the
# outbox, delivering, or advancing state and last_run.
DRY_RUN="${WEALTHFOLIO_DRY_RUN:-0}"

# msgvault is reached through the mcpproxy gateway in Claude Code's managed MCP
# config (/etc/claude-code/managed-mcp.json). Claude Code rejects a per-run
# --mcp-config whenever a managed config is present ("You cannot dynamically
# configure MCP servers when an enterprise MCP config is present"). Map this
# run's archive to its mcpproxy upstream server name.
case "$MSGVAULT_HOME" in
    *msgvault-work*) PROXY_SERVER="msgvault-work" ;;
    *)               PROXY_SERVER="msgvault-personal" ;;
esac

for f in "$CONFIG_FILE" "$RULES_FILE"; do
    if [[ ! -f "$f" ]]; then
        echo "ERROR: required file not found: $f"
        exit 1
    fi
done

mkdir -p "$SYNC_DIR"

if [[ ! -f "$STATE_FILE" ]]; then
    printf '{"processedSourceMessageIds":[]}\n' > "$STATE_FILE"
fi

LOOKBACK_DAYS="$(jq -r '.lookback_days_on_first_run // 120' "$CONFIG_FILE")"
OUTBOX_DIR="$(jq -r '.output_dir' "$CONFIG_FILE")"
mkdir -p "$OUTBOX_DIR"

if [[ -f "$LAST_RUN_FILE" ]]; then
    LAST_RUN="$(cat "$LAST_RUN_FILE")"
else
    LAST_RUN="$(date -u -d "$LOOKBACK_DAYS days ago" '+%Y-%m-%dT%H:%M:%SZ')"
    echo "No last_run file found, defaulting to: $LAST_RUN"
fi

NOW="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
AFTER_DATE="${LAST_RUN%T*}"

CONFIG_JSON="$(jq -c '.' "$CONFIG_FILE")"
TMP_OUTPUT="$(mktemp)"
TMP_FILTERED="$(mktemp)"
TMP_PARSED="$(mktemp)"
trap 'rm -f "$TMP_OUTPUT" "$TMP_FILTERED" "$TMP_PARSED"' EXIT

read -r -d '' PROMPT <<EOF || true
Extract executed trade recap emails for Wealthfolio import.

Configuration:
$CONFIG_JSON

Tool access: msgvault is the mcpproxy upstream server named "$PROXY_SERVER".
- First call mcp__mcpproxy__retrieve_tools with query "$PROXY_SERVER search messages"
  to load the exact tool names and input schemas.
- Call search_messages and get_message through mcp__mcpproxy__call_tool_read
  with name="$PROXY_SERVER:<tool>" and the arguments from that tool's schema.

Instructions:
1. For each rule in configuration.rules, search messages using its query plus after:$AFTER_DATE. Page through all results.
2. Read candidate messages in full with get_message.
3. Extract only executed trades from the newest message content, per the system rules.
4. Return strict JSON only in the exact envelope format described in the system prompt.
5. Use the rule's wealthfolio_account_id as accountId for extracted activities.
6. If there are no executed trades, return {"envelopes":[]}.
EOF

echo "Wealthfolio order sync: $LAST_RUN → $NOW"
echo "MSGVAULT_HOME: $MSGVAULT_HOME"
echo "Outbox: $OUTBOX_DIR"
echo "mcpproxy upstream: $PROXY_SERVER"

set +e
claude -p \
    --model sonnet \
    --max-turns 40 \
    --allowedTools "mcp__mcpproxy__retrieve_tools,mcp__mcpproxy__describe_tool,mcp__mcpproxy__call_tool_read,mcp__mcpproxy__read_cache" \
    --permission-mode dontAsk \
    --no-session-persistence \
    --append-system-prompt-file "$RULES_FILE" \
    "$PROMPT" > "$TMP_OUTPUT"
EXIT_CODE=$?
set -e

if [[ $EXIT_CODE -ne 0 ]]; then
    echo "ERROR: claude exited with code $EXIT_CODE"
    exit $EXIT_CODE
fi

extract_first_json() {
    python3 - "$1" <<'PY'
import json
import sys

text = open(sys.argv[1], "r", encoding="utf-8").read()
decoder = json.JSONDecoder()

for idx, ch in enumerate(text):
    if ch not in "{[":
        continue
    try:
        obj, _ = decoder.raw_decode(text[idx:])
    except Exception:
        continue
    print(json.dumps(obj))
    sys.exit(0)

sys.exit(1)
PY
}

if ! extract_first_json "$TMP_OUTPUT" > "$TMP_PARSED"; then
    echo "ERROR: extractor output did not contain a parseable JSON object"
    cat "$TMP_OUTPUT"
    exit 1
fi

if ! jq -e '.envelopes | type == "array"' "$TMP_PARSED" >/dev/null 2>&1; then
    echo "ERROR: extractor output is not valid envelope JSON"
    cat "$TMP_OUTPUT"
    exit 1
fi

jq \
    --slurpfile state "$STATE_FILE" \
    '
    .envelopes
    | map(select(
        (.sourceMessageId // "") as $id
        | ($state[0].processedSourceMessageIds // []) | index($id) | not
      ))
    ' "$TMP_PARSED" > "$TMP_FILTERED"

NEW_COUNT="$(jq 'length' "$TMP_FILTERED")"
echo "New executed-order envelopes: $NEW_COUNT"

if [[ "$DRY_RUN" == "1" ]]; then
    jq '.' "$TMP_FILTERED"
    echo "Dry run: nothing written, delivered, or recorded."
    exit 0
fi

sanitize_name() {
    printf '%s' "$1" \
        | tr ' /' '__' \
        | tr -cd '[:alnum:]_.-'
}

write_envelopes() {
    jq -c '.[]' "$TMP_FILTERED" | while IFS= read -r envelope; do
        source_message_id="$(printf '%s' "$envelope" | jq -r '.sourceMessageId')"
        sent_at="$(printf '%s' "$envelope" | jq -r '.sentAt // "unknown-date"')"
        rule_name="$(printf '%s' "$envelope" | jq -r '.ruleName // "rule"')"
        stamp="${sent_at%%T*}"
        safe_rule="$(sanitize_name "$rule_name")"
        safe_source="$(sanitize_name "$source_message_id")"
        out_file="$OUTBOX_DIR/${stamp}_${safe_source}_${safe_rule}.json"

        printf '%s' "$envelope" \
            | jq 'del(.recordKey)' > "$out_file"

        echo "Wrote $out_file"
    done
}

update_state() {
    jq \
        --slurpfile new "$TMP_FILTERED" \
        '
        .processedSourceMessageIds =
          (
            ((.processedSourceMessageIds // []) + ($new[0] | map(.sourceMessageId)))
            | map(select(. != null and . != ""))
            | unique
          )
        ' "$STATE_FILE" > "${STATE_FILE}.tmp"
    mv -f "${STATE_FILE}.tmp" "$STATE_FILE"
}

deliver_outbox() {
    local target
    target="$(jq -r '.delivery.rsync_target // ""' "$CONFIG_FILE")"
    if [[ -z "$target" ]]; then
        return 0
    fi

    mapfile -t rsync_args < <(jq -r '.delivery.rsync_args[]? // empty' "$CONFIG_FILE")
    if [[ ${#rsync_args[@]} -eq 0 ]]; then
        rsync_args=(-a)
    fi

    echo "Running rsync to $target"
    rsync "${rsync_args[@]}" "$OUTBOX_DIR"/ "$target"
}

if [[ "$NEW_COUNT" -gt 0 ]]; then
    write_envelopes
    deliver_outbox
    update_state
fi

echo "$NOW" > "$LAST_RUN_FILE"
echo "Wealthfolio order sync complete. Updated last_run to $NOW"

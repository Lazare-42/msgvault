# Wealthfolio Order Sync

Extract executed trade confirmations from email using `msgvault` MCP + Claude,
then deliver JSON activity envelopes into Wealthfolio's `email-orders-inbox/`.

## Files

Runtime state lives under:

```text
$MSGVAULT_HOME/wealthfolio-orders/
  config.json
  mcp.json
  rules.md
  last_run
  state.json
```

Repo templates:

- `integrations/wealthfolio-order-sync/config.example.json`
- `scripts/wealthfolio-order-rules.md`
- `scripts/wealthfolio-order-sync.sh`

## Setup

```bash
mkdir -p "$MSGVAULT_HOME/wealthfolio-orders"
cp integrations/wealthfolio-order-sync/config.example.json "$MSGVAULT_HOME/wealthfolio-orders/config.json"
cp scripts/wealthfolio-order-rules.md "$MSGVAULT_HOME/wealthfolio-orders/rules.md"
cp "$MSGVAULT_HOME/triage/mcp.json" "$MSGVAULT_HOME/wealthfolio-orders/mcp.json"
```

Adjust `config.json`:

- `output_dir`
- `delivery.rsync_target`
- each rule's `wealthfolio_account_id`

## Run

```bash
scripts/wealthfolio-order-sync.sh "$MSGVAULT_HOME"
```

## Scope

First pass is intentionally narrow:

- executed trade recaps only
- excludes open limit orders
- excludes expired orders
- excludes quoted historical trades lower in the thread

The Wealthfolio server watches:

```text
<data-root>/email-orders-inbox/
```

and imports these envelopes with the normal `ActivityImport` duplicate checks.

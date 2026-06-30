# Wealthfolio Valuation Sync

Extract per-holding NAV valuations from email (Neuflize/ABN AMRO, SOFRA, Dyptique
Gestion, HSBC) using `msgvault` MCP + Claude, then deliver nav-envelope JSON into
Wealthfolio's NAV-healing inbox (`<data-root>/nav-inbox/`). This refreshes
illiquid, manually-priced holdings that no market provider can quote.

Companion to `wealthfolio-order-sync` (executed trades). Same mechanism; this one
emits valuations, not activities.

## Setup

```bash
mkdir -p "$MSGVAULT_HOME/wealthfolio-valuations"
cp integrations/wealthfolio-valuation-sync/config.example.json "$MSGVAULT_HOME/wealthfolio-valuations/config.json"
cp scripts/wealthfolio-valuation-rules.md "$MSGVAULT_HOME/wealthfolio-valuations/rules.md"
cp "$MSGVAULT_HOME/triage/mcp.json" "$MSGVAULT_HOME/wealthfolio-valuations/mcp.json"
```

Adjust `config.json`: `output_dir`, `delivery.rsync_target` (local nav-inbox or a
remote server), and the per-sender `rules[].query`. Discover senders with:
`GET /api/v1/search?q=valorisation` on the msgvault REST API.

## Run

```bash
scripts/wealthfolio-valuation-sync.sh "$MSGVAULT_HOME"
```

Schedule on the same cadence as the email account sync (e.g. cron `0 */6 * * *`).

## Format

Each delivered file is one Wealthfolio nav-envelope (version 1):

```json
{ "version": 1, "asOf": "2026-03-31", "source": "...",
  "prices": [ { "isin": "LU0187079347", "nav": 142.83, "currency": "EUR", "name": "..." } ] }
```

The watcher resolves each price to an asset by ISIN/name and writes a MANUAL
quote. Extra provenance fields (ruleName, sourceMessageId, sentAt) are used only
for dedup/filenames and ignored by the watcher.

## Limitation

Extraction reads message **body** text. Valuations that live only inside
attachments (xlsx/pdf) need msgvault to expose attachment text to the MCP layer;
until then those holdings are omitted rather than guessed.

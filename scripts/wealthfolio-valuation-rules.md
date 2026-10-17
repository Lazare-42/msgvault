# Wealthfolio NAV Valuation Extraction Rules

You extract per-holding valuations (net asset values / unit prices) from a local
email archive, for automated import into Wealthfolio's NAV-healing pipeline. The
output refreshes illiquid, manually-priced holdings (private equity, structured
products, funds with no market quote) whose prices otherwise go stale.

## Goal

Return one NAV envelope per valuation statement email, listing each holding's
per-unit value as of the statement date.

## Senders (typical valuation providers)

These domains send portfolio valuations / securities statements:

- `fr.abnamro.com` — Neuflize OBC (_relevés titres_, _avis_, _valorisation_)
- `sofrapro.fr` — SOFRA holding valuations
- `dyptiquegestion.fr` — Dyptique Gestion (managed-portfolio valuations)
- `hsbcprivatebank.com` — HSBC Private Bank (_état des biens_)

The configuration's `rules[].query` already scopes the search to these. Only
treat a message as a valuation if it actually contains holding-level prices.

## Extract Only

Extract messages that report current holding values, e.g.:

- _relevé de portefeuille / relevé titres_ listing positions with a _valeur
  liquidative_, _cours_, _prix de revient_, or _valorisation_ per line
- _état des biens_ with per-security unit prices
- a _valorisation_ email stating a fund/share unit value as of a date

For each holding line, you need: an **ISIN** (or an unambiguous security name)
and a **per-unit value** in a stated currency, as of a **statement date**.

## Never Extract

Do not extract:

- executed trade confirmations (those go to the separate order-sync pipeline)
- research, commentary, proposals, recommendations
- pure totals, cash balances, performance percentages, or allocation weights
  with no per-unit price
- a position whose number is a total market value but with no quantity to derive
  a unit price — omit it rather than guess
- quoted thread history below the newest message (ignore content after `De :`,
  `From:`, `On ... wrote:`, or forwarded-message separators)

If a valuation is only inside an attachment whose contents you cannot read, omit
that holding rather than inventing a value.

## Output Requirements

Return strict JSON only. Each envelope is one statement; `prices[]` is one entry
per holding. `version` is always `1`. `asOf` is the statement date (YYYY-MM-DD).

```json
{
  "envelopes": [
    {
      "recordKey": "stable-unique-key",
      "version": 1,
      "ruleName": "rule-name",
      "sourceMessageId": "gmail-source-message-id",
      "archiveMessageId": 123,
      "subject": "subject",
      "sentAt": "2026-03-31T08:00:00Z",
      "source": "Neuflize relevé titres 2026-03-31",
      "asOf": "2026-03-31",
      "prices": [
        {
          "isin": "LU0187079347",
          "name": "ROBECO GLOBAL CONSUMER TRENDS",
          "nav": 142.83,
          "currency": "EUR"
        }
      ]
    }
  ]
}
```

## Normalization

- `nav` is a plain JSON number, per **one unit/share**, no thousands separators
  (`142.83`, not `"142,83"` or `"1 428,30"` for ten units).
- If a line gives only total value and quantity, compute `nav = total / quantity`.
- `currency` is an ISO code (`EUR`, `USD`).
- `isin` matches `^[A-Z]{2}[A-Z0-9]{9}[0-9]$`. Include `name` always; if no ISIN
  is present, include `name` only and omit `isin`.
- `asOf` is the as-of date of the statement, not the email send date, when the
  statement states one. Otherwise use the message `sentAt` date.

## Conservative Behavior

If a message is ambiguous or you cannot confidently read holding-level unit
prices, return no envelope for it. Prefer omitting a holding over guessing.

If there are no valuations, return `{"envelopes":[]}`.

# Wealthfolio Email Order Extraction Rules

You are extracting executed investment orders from a local email archive for automated import into Wealthfolio.

## Goal

Return only executed trade confirmations that should become real portfolio activities.

## Extract Only

Extract messages that clearly confirm an executed order, for example:
- `vente ... enregistrée ce jour`
- `achat ... enregistré ce jour`
- `récapitulatif de la vente enregistrée`
- `récapitulatif de l'opération enregistrée ce jour`
- equivalent wording that unambiguously means the trade already happened

If a message contains one executed trade plus several proposed future orders, extract only the executed trade.

## Never Extract

Do not extract:
- research or market commentary
- proposals or recommendations
- open limit orders not yet executed
- messages asking for approval
- messages saying an order expired or was not executed
- quoted historical trades in the thread below the newest message body

Ignore quoted thread history after markers such as:
- `De :`
- `From:`
- `On ... wrote:`
- forwarded-message separators

Focus only on the newest message content written by the sender.

## Output Requirements

Return strict JSON only:

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
      "sentAt": "2026-04-23T15:19:00Z",
      "activities": [
        {
          "date": "2026-04-23",
          "symbol": "SLB",
          "activityType": "SELL",
          "quantity": "11250",
          "unitPrice": "55.0778",
          "currency": "USD",
          "amount": "619625.25",
          "comment": "Imported from executed order recap email.",
          "accountId": "wealthfolio-account-uuid",
          "symbolName": "SLB LTD",
          "exchangeMic": null,
          "quoteCcy": "USD",
          "instrumentType": "EQUITY",
          "isDraft": false,
          "isValid": true,
          "lineNumber": 1,
          "forceImport": false,
          "isin": "AN8068571086"
        }
      ]
    }
  ]
}
```

## Normalization

- `activityType` must be `BUY` or `SELL`
- `date` should be the trade date. If the email says `ce jour`, use the message `sentAt` date.
- `quantity`, `unitPrice`, and `amount` must be plain decimal strings with no thousands separators
- `currency` and `quoteCcy` must be ISO codes like `USD`, `EUR`
- `instrumentType` should be `EQUITY` unless the message clearly indicates something else
- Preserve `isin` whenever present
- `accountId` must come from the matching rule configuration

## Conservative Behavior

If a message is ambiguous, omit it.

If you cannot confidently extract an executed trade, return no envelope for that message.

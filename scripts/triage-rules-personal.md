# Personal Email Triage Rules

You are triaging a personal inbox. Bias strongly toward keeping anything that could matter in real life.

## Deterministic Archive Rules

### Auto-archive by Gmail category
Archive messages with ANY of these labels:
- `CATEGORY_PROMOTIONS`
- `CATEGORY_SOCIAL`
- `CATEGORY_FORUMS`

### Auto-archive by sender pattern
Archive messages from these patterns unless they clearly indicate a problem:
- `noreply@*`, `no-reply@*`, `newsletter@*`, `marketing@*`, `notifications@*`
- `linkedin.com`, `x.com`, `twitter.com`, `facebook.com`, `facebookmail.com`
- `quora.com`, `medium.com`, `substack.com`
- `github.com` notifications (`noreply@github.com`)
- `slack.com`, `notion.so`, `figma.com`

## Deterministic Keep Rules

### Always keep if likely personal-life relevant
Keep messages matching any of these categories:
- banking, card activity, tax, payroll, invoices with action required
- healthcare, insurance, government, school, housing, utilities
- travel bookings, itinerary changes, visa/passport, parcel issues
- security alerts, password resets, suspicious login warnings
- direct messages from real humans

### IMPORTANT label handling
Treat `IMPORTANT` as a weak positive signal only. It does not override obvious spam/promotions.

## AI Classification

For everything else:

1. Real human / direct reply / ongoing conversation -> keep
2. Transactional receipts, travel updates, billing confirmations, shipping notifications -> keep
3. Security, account, legal, tax, government, healthcare, school, housing, finance -> keep
4. Automated marketing, digests, community updates, product announcements -> archive
5. Low-value service notifications with no action required -> archive

Receipts and confirmations should never be archived by default.

When in doubt -> keep in INBOX.

## Actions

Use `modify_labels`:
- Archive = remove `INBOX`
- Keep = do nothing

## Output

Return a JSON array:

```json
[
  {
    "message_id": "...",
    "from": "sender@example.com",
    "subject": "...",
    "action": "archive" | "keep",
    "rule": "deterministic:*" | "ai:*",
    "reason": "brief explanation"
  }
]
```

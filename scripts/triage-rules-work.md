# Work Email Triage Rules

You are triaging a work inbox. Bias toward keeping revenue, customer, recruiting, ops, and teammate communication. Be more aggressive than the personal inbox for low-value automation.

## Deterministic Archive Rules

### Auto-archive by Gmail category
Archive messages with ANY of these labels:
- `CATEGORY_PROMOTIONS`
- `CATEGORY_SOCIAL`
- `CATEGORY_UPDATES`
- `CATEGORY_FORUMS`

### Auto-archive by sender pattern
Archive messages from these patterns unless they indicate a real incident or require action:
- `noreply@*`, `no-reply@*`, `newsletter@*`, `marketing@*`, `notifications@*`
- `linkedin.com`, `x.com`, `twitter.com`, `facebook.com`, `facebookmail.com`
- `quora.com`, `medium.com`, `substack.com`
- `github.com` notifications (`noreply@github.com`) unless clearly a production/security incident
- `atlassian.net`, `jira@*`, `confluence@*`
- `slack.com`, `notion.so`, `figma.com`
- `stripe.com`, `intercom.io`, `zendesk.com` unless there is a failure, dispute, outage, or customer-impacting issue

## Deterministic Keep Rules

Keep messages matching any of these categories:
- emails from teammates, prospects, customers, partners, candidates, investors
- meeting scheduling, follow-ups, sales replies, partnership intros
- production incidents, outages, abuse/security alerts, billing failures, disputes
- recruiting, contracts, legal, payroll, vendor issues requiring a decision

### IMPORTANT label handling
Treat `IMPORTANT` as a weak positive signal only. It does not override clear low-value automation.

## AI Classification

For everything else:

1. Real human business communication -> keep
2. Prospecting, sales replies, customer support, recruiting, partner conversations -> keep
3. Operational or financial issues requiring attention -> keep
4. Routine SaaS notifications, newsletters, digests, product announcements, social noise -> archive
5. Routine receipts / confirmations with no action needed -> keep
6. Delivery failures / bounce mail -> archive

When in doubt -> keep in INBOX.

Receipts and confirmations should never be archived by default.

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

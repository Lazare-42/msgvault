---
title: Triage Queue
description: Move stale untreated messages from a label pool into a queue folder with msgvault triage-queue.
---

# Triage Queue

`msgvault triage-queue` is a deterministic primitive for inbox triage: it scans
a label pool (default `INBOX`) for messages older than a staleness cutoff,
skips everything already treated, and moves the rest into a queue folder on
the mail server. Run it from cron or a script to keep an inbox down to the
messages that still need attention.

```bash
# Preview: what would move after 2 full working days without treatment?
msgvault triage-queue --older-than-workdays 2 --move-to "Queue" --dry-run

# Move stale untreated mail into the "Queue" folder
msgvault triage-queue --account user@example.com \
  --older-than-workdays 2 \
  --not-label handled \
  --move-to "Queue"
```

The command reads the archive through the msgvault daemon (which must be
reachable, like other daemon-backed commands) and executes moves against the
live mailbox using the account's stored credentials. Moves use the IMAP
folder-move primitive, so the account must be an IMAP-backed source (plain
IMAP or Microsoft 365); `--dry-run` works for any source type.

## Staleness cutoff

Exactly one of the two cutoff flags is required:

- `--before YYYY-MM-DD` — absolute date; messages sent before it are stale.
  The date is interpreted as **UTC midnight**.
- `--older-than-workdays N` — messages are stale once `N` full working days
  (Monday-Friday) have passed; weekends do not count. Run on a Monday with
  `--older-than-workdays 2`, Friday mail is not yet stale (only one full
  working day has passed) but Wednesday mail is. Day boundaries are
  **midnight in the local timezone** of the machine running the command.

## What gets skipped

- **Wrong source mailbox** — for IMAP accounts, the mailbox encoded in each
  canonical `mailbox|UID` source ID must match `--label`. Historical labels
  retained after deduplication cannot move a message out of an archive, sent,
  deleted, or other physical folder. These are counted as
  `skipped_wrong_mailbox`.
- **Treated labels** — `--not-label <name>` (repeatable): messages carrying
  any of these labels are considered handled. Matching is case-insensitive
  but exact per label, so a treated label `handled` does not match a folder
  named `handled items`. IMAP keywords (e.g. Outlook categories) arrive in
  the archive as labels, so marking a message with a category is enough to
  exclude it.
- **Already queued** — messages already carrying the `--move-to` folder label
  are never re-moved; the destination is always auto-excluded.
- **Skip IDs** — `--skip-ids 12,57` excludes specific archive message IDs,
  typically ones a previous `--report-replied` review judged as treated.
  These are counted separately in the report as `skipped_by_id`.

## Replied messages

With `--report-replied`, messages carrying the `ANSWERED` label (the IMAP
`\Answered` flag) are not moved. Instead they are listed in the JSON report,
each with the text of the latest reply sent from one of the account's own
addresses (the account identifier plus its recorded identities; see
`msgvault identity list`), truncated to 600 characters. An external reviewer
— human or script — decides whether each one was really handled and feeds the
treated ones back via `--skip-ids` on the next run. When no own reply is
found in the conversation, `reply_text` is empty and the decision is the
caller's.

## Output

Progress goes to stderr; stdout carries a single JSON object:

```json
{
  "cutoff": "2026-08-06T00:00:00Z",
  "scanned": 42,
  "moved": [101, 102],
  "move_errors": 0,
  "move_noops": 1,
  "skipped_wrong_mailbox": 4,
  "skipped_treated": 7,
  "skipped_by_id": 2,
  "skipped_already_queued": 3,
  "replied": [
    {
      "id": 117,
      "source_message_id": "INBOX|4711",
      "subject": "Quick question",
      "reply_text": "Thanks, we shipped the fix yesterday..."
    }
  ],
  "dry_run": false
}
```

`moved` lists archive message IDs (with `--dry-run` it lists the messages that
would move). Live moves execute one at a time: only server-confirmed moves enter
`moved`, stale or ambiguous source UIDs increment `move_noops`, and individual
failures increment `move_errors`. Processing continues after a per-message
failure unless the command context is cancelled.

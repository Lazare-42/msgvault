# Wealthfolio Sync

Standalone mail-to-PDF handoff for Wealthfolio.

This tool runs next to a local `msgvault` archive, searches matching messages by
rule, exports only unseen PDF attachments, and writes a sidecar JSON file that
includes the mapped Wealthfolio account ID.

The result is an outbox directory that can be copied to the Wealthfolio server,
for example with `rsync`, without teaching Wealthfolio how to read mailboxes.

## What it does

- Searches the local `msgvault` archive with Gmail-like queries
- Filters attachments down to PDFs
- Deduplicates by `source_message_id + attachment content hash`
- Writes each PDF into an outbox directory
- Writes a sidecar `.json` file with Wealthfolio account mapping metadata
- Optionally runs `rsync` to a remote Wealthfolio spool directory

## Usage

```bash
go run ./integrations/wealthfolio-sync --config /path/to/config.toml
go run ./integrations/wealthfolio-sync --config ./integrations/wealthfolio-sync/config.example.toml --dry-run
```

## Config

Start from [config.example.toml](./config.example.toml).

Important fields:

- `msgvault_home`: local archive root, defaults to `~/.msgvault`
- `output_dir`: local outbox where PDFs and sidecars are written
- `state_file`: persisted seen-state used for attachment dedupe
- `delivery.rsync_target`: optional remote spool directory such as
  `wealthfolio@prod:/srv/wealthfolio/maildrop/incoming/`

Each `[[rule]]` maps a mailbox query to a Wealthfolio account:

- `query`: Gmail-like `msgvault` search query
- `msgvault_account`: optional `msgvault` source account identifier
- `wealthfolio_account_id`: target Wealthfolio account ID
- `attachment_filename_regex`: optional extra PDF filename filter
- `subject_regex`: optional subject filter
- `from_regex`: optional sender/name filter

## Output

For each exported PDF, the tool writes:

- `YYYY-MM-DD_<hash12>_<sanitized-name>.pdf`
- `YYYY-MM-DD_<hash12>_<sanitized-name>.json`

The sidecar JSON includes:

- `wealthfolioAccountId`
- `ruleName`
- `sourceMessageId`
- `attachmentContentHash`
- sender, recipient, subject, and timestamp metadata

## Wealthfolio handoff

The intended flow is:

1. Run this tool on the machine that hosts `msgvault`
2. Copy the outbox to a remote Wealthfolio spool directory
3. On the Wealthfolio host, atomically move PDFs into `pdf-inbox/`
4. Teach Wealthfolio to read the sidecar and preselect the account

This tool only guarantees attachment-level dedupe. Wealthfolio still needs its
own activity-level duplicate detection before import confirmation.

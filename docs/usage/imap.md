---
title: IMAP Folder Sync
description: List IMAP folders and choose which folders msgvault scans during a sync.
---

# IMAP Folder Sync

By default, msgvault scans every selectable folder in an IMAP account. You can
limit a sync to the folders you need or skip folders that are large or
unimportant. This is useful when you want to try msgvault with a small part of
an account before starting a complete archive.

Folder filters work with both `sync-full` and `sync`. They affect IMAP accounts
only.

## Find the Folder Names

Ask the IMAP server for its folder names before creating a filter:

```bash
msgvault list-folders you@example.com
```

The command shows each selectable folder and its approximate message count:

```text
Account: you@example.com

  Folder                                Messages
  ----------------------------------------------
  INBOX                                     1240
  Archive                                  18342
  Projects/Alpha                             217
  Trash                                       36
```

Leave out the account name to list folders for every configured IMAP account:

```bash
msgvault list-folders
```

Some servers do not provide a message count for every folder. In that case,
msgvault shows `??`, but you can still use the folder name in a filter.

## Sync Only Selected Folders

Repeat `--folder` once for each folder you want to include:

```bash
msgvault sync-full you@example.com \
  --folder INBOX \
  --folder Archive
```

To scan the same folders during a later sync:

```bash
msgvault sync you@example.com \
  --folder INBOX \
  --folder Archive
```

Each flag takes one complete folder name. Repeat the flag instead of joining
names with commas. This also means a folder whose name contains a comma works
without special handling:

```bash
msgvault sync-full you@example.com --folder "Receipts, 2025"
```

## Skip Selected Folders

Use `--skip-folder` to scan every folder except the ones you name:

```bash
msgvault sync-full you@example.com \
  --skip-folder Trash \
  --skip-folder Spam
```

You can combine include and exclude filters. msgvault first keeps the folders
named by `--folder`, then removes any named by `--skip-folder`:

```bash
msgvault sync-full you@example.com \
  --folder INBOX \
  --folder Archive \
  --folder "Archive/Newsletters" \
  --skip-folder "Archive/Newsletters"
```

That example scans `INBOX` and `Archive`.

## Matching Rules

- Folder names are matched exactly, without wildcards or prefix matching.
- Matching is case-insensitive.
- Nested folders use the full name shown by `list-folders`, such as
  `Projects/Alpha`.
- With no folder flags, msgvault scans every selectable folder.
- Folder flags apply to one command invocation. Repeat them in later commands
  when you want the same filter.
- If a command syncs several account types, folder flags affect only its IMAP
  accounts.

## What Filtering Changes

A folder filter limits which remote IMAP folders msgvault scans during that
run. It does not delete messages from the server or remove messages already in
the local archive.

An email can appear in more than one IMAP folder. During a filtered scan,
msgvault keeps the stable identity and folder labels learned by earlier,
broader scans while adding information from the selected folders. A later sync
without folder flags scans the complete account again.

Folder filtering works the same whether the CLI uses a local daemon or a
configured remote msgvault server.

## Flag-Derived Labels

Besides folder names, msgvault stores each message's IMAP flags as searchable
labels:

- `UNREAD` — the message has no `\Seen` flag.
- `STARRED` — the message carries the `\Flagged` flag.
- `ANSWERED` — the message carries the `\Answered` flag (you replied to it).
- Custom IMAP keywords appear verbatim as labels. Outlook categories arrive
  this way: a category named `Traite` becomes a `Traite` label. Some servers
  store keywords lowercased, so the label may appear as `traite`.

Internal client bookkeeping is not turned into labels: system flags such as
`\Draft` and `\Deleted`, `$`-prefixed keywords such as `$Forwarded`, and bare
spam-training keywords such as `NonJunk` are skipped.

Flag labels track the server on every sync, including folder-filtered ones.
Reading a message removes its `UNREAD` label on the next sync, replying adds
`ANSWERED`, and removing a category drops the matching keyword label. This
makes searches such as "in INBOX, not replied, without the Traite category"
possible from the TUI or query interface.

## Filing Messages Into Folders

IMAP has no multi-label model: a message lives in exactly one mailbox, so
"labeling" over IMAP is a MOVE. The `modify_labels` MCP tool accepts these
labels for IMAP and Microsoft 365 accounts:

- `UNREAD` — add to mark unread, remove to mark read (`\Seen`).
- `STARRED` — add/remove the `\Flagged` flag (a "pin").
- `INBOX` — add to move back to INBOX, remove to archive.
- `folder:<name>` — add to MOVE the message into the named mailbox, e.g.
  `folder:Recruiting`. The mailbox is created on demand if it does not exist.
- `keyword:<name>` — add/remove an IMAP keyword (custom flag) on the message,
  e.g. `keyword:Traite`. Exchange and Microsoft 365 surface keywords as
  Outlook categories; servers may canonicalize the keyword's case. Keywords
  combine freely with other flags and with a folder/INBOX move in one call.
  The name must be a single IMAP flag atom: accented/UTF-8 letters are fine,
  but spaces and the special characters `( ) { } % * " \ ]` are rejected.

A `folder:` move is mutually exclusive with an INBOX add/remove in the same
call, but a flag (e.g. `STARRED`) can be applied alongside it. Removing a
`folder:` label is not supported — move to a different folder instead. The
`create_label` tool pre-provisions an empty mailbox without moving anything.

## Applying Labels From the CLI

The same label operations are available without MCP through the
`modify-labels` command:

```bash
msgvault modify-labels --account user@example.com --ids 12,34 \
  --add "keyword:Handled" --remove UNREAD
```

`--ids` takes archive message IDs (the numeric IDs shown by the TUI and
query interface, resolved through the msgvault daemon); `--source-ids`
takes the mail server's own message IDs and passes them through as-is.
At least one of `--ids`/`--source-ids` and at least one of
`--add`/`--remove` is required. Writes are applied in chunks and require
an IMAP-backed account; `--dry-run` previews the operation for any
account type. The command prints a single JSON report on stdout
(`{"modified": [...], "errors": N, "dry_run": ...}`) with progress on
stderr.

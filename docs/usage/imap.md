---
last_edited: "2026-09-15"
title: IMAP Sync and Repair
description: Archive IMAP mail efficiently, choose folders, and repair stored labels.
---

Archive mail from an IMAP account, then keep it current without downloading
unchanged messages again. Start with [IMAP account setup](/docs/setup/#add-an-imap-account)
if you have not connected the account yet.

```bash
msgvault sync-full you@example.com
msgvault sync you@example.com
```

Sync reads the provider. It preserves messages already in your local archive
when their server copies disappear. Remote deletion is a separate
[staged workflow](/docs/usage/deletion/).

## How later syncs find changes

msgvault chooses the sync method from the server's capabilities:

| Server behavior | What msgvault does |
|---|---|
| Supports QRESYNC, the IMAP change-tracking extension | Uses saved mailbox state to fetch changes and track messages removed from folders |
| Does not support QRESYNC | Compares mailbox counts and saved message-number boundaries; skips unchanged folders and fetches new messages where possible |
| State is missing, inconsistent, or no longer valid | Enumerates the affected folders again to establish current membership |

A failed QRESYNC attempt reconnects and falls back to full enumeration. An
incomplete server response is not accepted as a complete mailbox snapshot.
Temporary connection failures receive bounded retries; a persistent failure
still ends the run with an error. Rerun the command after correcting the
connection problem.

## Choose folders

By default, msgvault scans every selectable folder. Folder filters let you
start with a small part of an account or leave out folders you do not need.
They work with both `sync-full` and `sync` and affect IMAP sources only.

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

## Repair stored labels

Use `repair-labels` when an archived message still shows a folder label that
no longer belongs to it. The command rebuilds labels from the folder
memberships already stored in msgvault. It does not contact the provider.

1. Preview the repair for one source:

    ```bash
    msgvault repair-labels you@example.com
    ```

2. Review the `scanned` and `changed` counts, then apply it:

    ```bash
    msgvault repair-labels you@example.com --apply
    ```

Omit the identifier to check or repair every IMAP source. Applying a repair
also refreshes the analytical cache.

A sync with incomplete folder information only adds labels; it does not
remove labels it cannot disprove. If the stored memberships later become
complete but never change again, an old label can remain until this repair.

If the stored memberships themselves need refreshing, enumerate the server
again first:

```bash
msgvault sync-full you@example.com --noresume
```

Leave out folder filters for a complete account scan. `repair-labels` cannot
recover memberships that the archive has never observed.

## Reply drafts

Create a reply in your IMAP Drafts folder, then review and send it from your
usual mail application. Msgvault never sends email. Draft creation is disabled
until an operator grants it for one exact IMAP source on the daemon host.

1. Run `msgvault list-accounts` to find the source ID. Confirm the Drafts
    folder's exact name with `msgvault list-folders <account>`.

1. Add the grant to the daemon host's `config.toml`, using that source ID and
    folder name:

    ```toml
    [[imap.drafts]]
    source_id = 42
    enabled = true
    mailbox = "Drafts"
    ```

1. Restart the daemon. The host policy applies per source. Owner callers
    using an API key, browser session, or keyless loopback can create drafts
    on a granted source. Delegated callers also need that source in their
    [agent token grant](../cli-reference.md#agent-token). Client configuration,
    request fields, and environment variables cannot grant access or choose a
    different folder.

1. Check the source's confirmed sender identities:

    ```bash
    msgvault identity list --source-id 42
    ```

    If your address is missing, confirm it with
    `msgvault identity add --source-id 42 you@example.com`.

1. Find the parent email's local message ID with search, then create the draft:

    ```bash
    msgvault draft-reply 123 --from you@example.com \
      --body 'Thanks for the update. I will review it tomorrow.' --json
    ```

The parent must belong to the granted IMAP source and have its original email
stored in the archive. Msgvault composes a plain-text reply using the parent's
threading headers. `--from` must be a confirmed identity for that source, and
`--body` is required; `--body=` creates an empty draft. The IMAP server must
support UIDPLUS, which returns a receipt that identifies the stored draft.

A successful result reports `status: "created"`, the archived `message_id`, and
the remote mailbox receipt. The draft is marked `\Draft` and stored locally with
its original email content. Later syncs advance the mailbox cursor and reconcile
any mailbox identifier changes.

### If draft creation does not finish

The command does not retry the remote write automatically. Use the reported
outcome to decide what to do next:

| Result                                      | Next step                                                                                                                                            |
| ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sync_active`                               | Wait for this source's sync to finish, then retry.                                                                                                   |
| `uidplus_required`                          | Use a server that advertises UIDPLUS; no draft was appended.                                                                                         |
| `append_rejected`                           | Check that the configured folder exists and permits writes.                                                                                          |
| `remote_unknown` or `accepted_unidentified` | Inspect the Drafts folder before retrying; the draft may already exist.                                                                              |
| `remote_accepted_local_failed`              | The server accepted the draft, but the local save failed. Use the reported `operation_ref` and mailbox receipt to inspect it before another request. |

## Keep edited outgoing mail current

After you edit or send a draft in your mail application, IMAP sync can update
its archived body, recipients, attachments, and search text while retaining the
local message ID. A newer Sent copy takes precedence over a stale Drafts copy.

This replacement is limited to trusted outgoing folders. Msgvault trusts
unambiguous server-advertised `\Sent` and `\Drafts` roles. If your server does
not advertise Sent correctly, configure the exact account and folder under
[`sync.trusted_imap_sent_mailboxes`](../configuration.md#sync). Only name
folders used for sent mail; never include folders that receive incoming mail
through filters or filing rules. Ordinary received-mail and All Mail copies
preserve the archived content instead of replacing it.

These rules apply during later syncs. They do not automatically repair older
archive rows that already lost the location information needed to identify the
outgoing copy.

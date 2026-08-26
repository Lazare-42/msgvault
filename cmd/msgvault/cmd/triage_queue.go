package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/triage"
)

var (
	triageAccount       string
	triageLabel         string
	triageBefore        string
	triageOlderWorkdays int
	triageNotLabels     []string
	triageMoveTo        string
	triageReportReplied bool
	triageSkipIDs       string
	triageDryRun        bool
)

const (
	triageFlagBefore        = "before"
	triageFlagOlderWorkdays = "older-than-workdays"
	// triagePageSize is the archive scan page size (kept under the API
	// server's page cap so a short page reliably means "last page").
	triagePageSize = 200
	// triageReplyTextLimit caps reply_text in the JSON report.
	triageReplyTextLimit = 600
)

var triageQueueCmd = &cobra.Command{
	Use:   "triage-queue",
	Short: "Move stale untreated messages from a label into a queue folder",
	Long: `Move stale untreated messages from a label pool into a queue folder.

Scans the archive (through the msgvault daemon) for messages in --label
older than a staleness cutoff. For IMAP accounts, the canonical source
mailbox must also match --label; a historical label retained after
deduplication cannot move a message out of another mailbox. Messages
already treated (any --not-label, repeatable, matched case-insensitively)
or already in the queue folder are skipped, and the rest move to
--move-to on the mail server.

The cutoff is either an absolute date (--before) or a number of full
working days Mon-Fri (--older-than-workdays): run on a Monday with
--older-than-workdays 2, Friday mail is not yet stale but Wednesday
mail is. Exactly one of the two must be given.

With --report-replied, messages carrying the ANSWERED label are not
moved; they are listed in the JSON report together with the text of the
latest reply sent from one of the account's own addresses, so an
external reviewer can decide whether they were really handled. Pass the
resulting archive IDs back via --skip-ids on the next run to exclude
the ones judged treated.

Moves use the IMAP folder-move primitive, so the account must be an
IMAP-backed source (plain IMAP or O365). --dry-run classifies and
reports without moving and works for any source type.

The result is a single JSON object on stdout; progress goes to stderr.

Examples:
  msgvault triage-queue --older-than-workdays 2 --move-to "Queue" --dry-run
  msgvault triage-queue --account user@example.com --label INBOX \
    --older-than-workdays 2 --not-label handled --move-to "Queue"
  msgvault triage-queue --before 2026-01-01 --move-to "Queue" \
    --report-replied --skip-ids 12,57`,
	Args: cobra.NoArgs,
	RunE: runTriageQueue,
}

// triageRepliedEntry is one withheld ANSWERED message in the report.
type triageRepliedEntry struct {
	ID              int64  `json:"id"`
	SourceMessageID string `json:"source_message_id"`
	Subject         string `json:"subject"`
	ReplyText       string `json:"reply_text"`
}

// triageReport is the single JSON object emitted on stdout.
type triageReport struct {
	Cutoff               string               `json:"cutoff"`
	Scanned              int                  `json:"scanned"`
	Moved                []int64              `json:"moved"`
	MoveErrors           int                  `json:"move_errors"`
	MoveNoops            int                  `json:"move_noops"`
	SkippedWrongMailbox  int                  `json:"skipped_wrong_mailbox"`
	SkippedTreated       int                  `json:"skipped_treated"`
	SkippedByID          int                  `json:"skipped_by_id"`
	SkippedAlreadyQueued int                  `json:"skipped_already_queued"`
	Replied              []triageRepliedEntry `json:"replied"`
	DryRun               bool                 `json:"dry_run"`
}

type triageFolderMover interface {
	MoveMessageToFolder(ctx context.Context, messageID, folder string) (bool, error)
}

func runTriageQueue(cmd *cobra.Command, _ []string) error {
	moveTo := strings.TrimSpace(triageMoveTo)
	if moveTo == "" {
		return usageErr(cmd, errors.New("--move-to is required"))
	}
	cutoff, err := triageCutoff(cmd)
	if err != nil {
		return usageErr(cmd, err)
	}
	skipIDs, err := parseTriageSkipIDs(triageSkipIDs)
	if err != nil {
		return usageErr(cmd, err)
	}

	ctx := cmd.Context()
	st, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return fmt.Errorf("open daemon: %w", err)
	}
	defer func() { _ = st.Close() }()
	engine := daemonclient.NewEngineAdapter(st)

	accounts, err := st.GetCLIAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	account, err := resolveSyncableAccount(accounts, triageAccount)
	if err != nil {
		return err
	}

	// Resolve the live mail client before the (potentially long) scan so
	// configuration problems surface immediately. Dry runs never touch
	// the mail server and skip this entirely.
	var apiClient gmail.API
	if !triageDryRun {
		src, err := lookupAccountWriteSource(account)
		if err != nil {
			return err
		}
		if src.SourceType != sourceTypeIMAP {
			return fmt.Errorf(
				"account %s is a %s source; triage-queue moves use the IMAP folder primitive "+
					"and support only IMAP-backed sources (use --dry-run to classify without moving)",
				account.Email, src.SourceType)
		}
		apiClient, err = buildAPIClient(ctx, src, oauthManagerCache(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = apiClient.Close() }()
	}

	fmt.Fprintf(os.Stderr, "Scanning %s label %q for messages before %s...\n",
		account.Email, triageLabel, cutoff.Format(time.RFC3339))
	msgs, err := listTriageCandidates(ctx, engine, account.ID, triageLabel, cutoff)
	if err != nil {
		return fmt.Errorf("scan messages: %w", err)
	}
	scanned := len(msgs)
	skippedWrongMailbox := 0
	if account.Type == sourceTypeIMAP {
		msgs, skippedWrongMailbox = filterTriageSourceMailbox(msgs, triageLabel)
	}
	fmt.Fprintf(os.Stderr, "Scanned %d message(s); %d canonical source mailbox mismatch(es).\n",
		scanned, skippedWrongMailbox)

	res := triage.Classify(msgs, triage.Options{
		QueueFolder:   moveTo,
		TreatedLabels: parseFolderFilter(triageNotLabels),
		ReportReplied: triageReportReplied,
		SkipIDs:       skipIDs,
	})

	report := triageReport{
		Cutoff:               cutoff.Format(time.RFC3339),
		Scanned:              scanned,
		Moved:                []int64{},
		SkippedWrongMailbox:  skippedWrongMailbox,
		SkippedTreated:       res.SkippedTreated,
		SkippedByID:          res.SkippedByID,
		SkippedAlreadyQueued: res.SkippedAlreadyQueued,
		Replied:              []triageRepliedEntry{},
		DryRun:               triageDryRun,
	}

	if len(res.Replied) > 0 {
		report.Replied = triageRepliedEntries(ctx, st, engine, account.Email, res.Replied)
	}

	if triageDryRun {
		for _, m := range res.Move {
			report.Moved = append(report.Moved, m.ID)
		}
		fmt.Fprintf(os.Stderr, "Dry run: %d message(s) would be moved to %q.\n", len(report.Moved), moveTo)
		return printJSON(report)
	}

	mover, ok := apiClient.(triageFolderMover)
	if !ok {
		return errors.New("IMAP client does not report folder move results")
	}
	report.Moved, report.MoveNoops, report.MoveErrors = executeTriageMoves(ctx, mover, res.Move, moveTo)
	fmt.Fprintf(os.Stderr, "Moved %d message(s) to %q (%d no-op(s), %d error(s)).\n",
		len(report.Moved), moveTo, report.MoveNoops, report.MoveErrors)
	if err := printJSON(report); err != nil {
		return err
	}
	if report.MoveErrors > 0 {
		return fmt.Errorf("%d message(s) failed to move", report.MoveErrors)
	}
	return nil
}

// filterTriageSourceMailbox keeps only messages whose canonical composite
// source ID points at mailbox. IMAP deduplication can retain historical folder
// labels after a message moves, so label membership alone is unsafe for writes.
func filterTriageSourceMailbox(
	msgs []query.MessageSummary,
	mailbox string,
) ([]query.MessageSummary, int) {
	kept := make([]query.MessageSummary, 0, len(msgs))
	skipped := 0
	for _, msg := range msgs {
		sourceMailbox, ok := triageSourceMailbox(msg.SourceMessageID)
		if !ok || !strings.EqualFold(sourceMailbox, mailbox) {
			skipped++
			continue
		}
		kept = append(kept, msg)
	}
	return kept, skipped
}

func triageSourceMailbox(sourceMessageID string) (string, bool) {
	separator := strings.LastIndex(sourceMessageID, "|")
	if separator <= 0 || separator == len(sourceMessageID)-1 {
		return "", false
	}
	if _, err := strconv.ParseUint(sourceMessageID[separator+1:], 10, 32); err != nil {
		return "", false
	}
	return sourceMessageID[:separator], true
}

// triageCutoff derives the staleness cutoff from --before XOR
// --older-than-workdays.
func triageCutoff(cmd *cobra.Command) (time.Time, error) {
	beforeSet := triageBefore != ""
	workdaysSet := cmd.Flags().Changed(triageFlagOlderWorkdays)
	if beforeSet == workdaysSet {
		return time.Time{}, fmt.Errorf(
			"exactly one of --%s or --%s is required", triageFlagBefore, triageFlagOlderWorkdays)
	}
	if beforeSet {
		t, err := time.Parse("2006-01-02", triageBefore)
		if err != nil {
			return time.Time{}, fmt.Errorf(
				"invalid --%s date %q (expected YYYY-MM-DD): %w", triageFlagBefore, triageBefore, err)
		}
		return t, nil
	}
	if triageOlderWorkdays < 0 {
		return time.Time{}, fmt.Errorf("--%s must be non-negative", triageFlagOlderWorkdays)
	}
	return triage.CutoffWorkdays(time.Now(), triageOlderWorkdays), nil
}

// parseTriageSkipIDs parses the comma-separated --skip-ids value into a
// set of archive message IDs. Empty segments are ignored.
func parseTriageSkipIDs(raw string) (map[int64]bool, error) {
	ids := map[int64]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid --skip-ids entry %q (expected a numeric archive message ID)", part)
		}
		ids[id] = true
	}
	return ids, nil
}

// listTriageCandidates pages through the daemon's filtered message list
// for the label pool below the cutoff. The engine already applies the
// Before filter; the SentAt guard is defensive so classification can
// never move a message at or past the cutoff boundary.
//
// Offset paging over a live archive can skip or duplicate a message
// when the daemon ingests new mail mid-scan. That is acceptable here:
// runs are idempotent (already-queued messages are excluded) and the
// next run picks up anything a shifted page missed.
func listTriageCandidates(
	ctx context.Context,
	engine *daemonclient.Engine,
	sourceID int64,
	label string,
	cutoff time.Time,
) ([]query.MessageSummary, error) {
	var all []query.MessageSummary
	offset := 0
	for {
		filter := query.MessageFilter{
			Label:                 label,
			SourceID:              &sourceID,
			Before:                &cutoff,
			HideDeletedFromSource: true,
			Pagination:            query.Pagination{Limit: triagePageSize, Offset: offset},
			Sorting: query.MessageSorting{
				Field:     query.MessageSortByDate,
				Direction: query.SortAsc,
			},
		}
		page, err := engine.ListMessages(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, m := range page {
			if !m.SentAt.Before(cutoff) {
				continue
			}
			all = append(all, m)
		}
		if len(page) < triagePageSize {
			return all, nil
		}
		offset += len(page)
	}
}

// triageRepliedEntries builds the replied report entries. Failures to
// resolve a reply body degrade to an empty reply_text with a warning on
// stderr; the caller decides what an empty reply means.
func triageRepliedEntries(
	ctx context.Context,
	st *daemonclient.Client,
	engine *daemonclient.Engine,
	account string,
	replied []query.MessageSummary,
) []triageRepliedEntry {
	identities := triageIdentities(ctx, st, account)
	entries := make([]triageRepliedEntry, 0, len(replied))
	for _, m := range replied {
		replyText, err := triageReplyText(ctx, engine, m, identities)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: reply lookup for message %d failed: %v\n", m.ID, err)
			replyText = ""
		}
		entries = append(entries, triageRepliedEntry{
			ID:              m.ID,
			SourceMessageID: m.SourceMessageID,
			Subject:         m.Subject,
			ReplyText:       replyText,
		})
	}
	return entries
}

// triageIdentities returns the lowercase set of addresses that count as
// "sent by this account": the account identifier itself plus every
// identity address the daemon has recorded for it (msgvault identity
// list). Identity lookup failures degrade to the identifier alone.
func triageIdentities(ctx context.Context, st *daemonclient.Client, account string) map[string]bool {
	identities := map[string]bool{strings.ToLower(account): true}
	rows, err := st.GetCLIIdentities(ctx, daemonclient.CLIIdentitiesRequest{Account: account})
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: identity lookup for %s failed (%v); matching replies against the account address only\n",
			account, err)
		return identities
	}
	for _, row := range rows {
		addr := strings.ToLower(strings.TrimSpace(row.Identifier))
		if row.None || addr == "" {
			continue
		}
		identities[addr] = true
	}
	return identities
}

// triageReplyText returns the body text (fallback: snippet) of the
// latest message in m's conversation sent from one of the account's
// identities, truncated to triageReplyTextLimit runes. Returns "" when
// the conversation holds no own message.
func triageReplyText(
	ctx context.Context,
	engine *daemonclient.Engine,
	m query.MessageSummary,
	identities map[string]bool,
) (string, error) {
	if m.ConversationID == 0 {
		return "", nil
	}
	convID := m.ConversationID
	thread, err := engine.ListMessages(ctx, query.MessageFilter{ConversationID: &convID})
	if err != nil {
		return "", fmt.Errorf("list conversation %d: %w", convID, err)
	}

	var latest *query.MessageSummary
	for i := range thread {
		candidate := &thread[i]
		if !identities[strings.ToLower(strings.TrimSpace(candidate.FromEmail))] {
			continue
		}
		if latest == nil || candidate.SentAt.After(latest.SentAt) {
			latest = candidate
		}
	}
	if latest == nil {
		return "", nil
	}

	detail, err := engine.GetMessage(ctx, latest.ID)
	if err != nil {
		return "", fmt.Errorf("fetch reply body %d: %w", latest.ID, err)
	}
	text := ""
	if detail != nil {
		text = strings.TrimSpace(detail.BodyText)
	}
	if text == "" {
		text = strings.TrimSpace(latest.Snippet)
	}
	return triage.TruncateText(text, triageReplyTextLimit), nil
}

// executeTriageMoves moves msgs into folder one at a time so the report can
// distinguish confirmed moves from stale source UIDs and per-message errors.
func executeTriageMoves(
	ctx context.Context,
	client triageFolderMover,
	msgs []query.MessageSummary,
	folder string,
) (moved []int64, moveNoops, moveErrors int) {
	moved = []int64{}
	for i, msg := range msgs {
		confirmed, err := client.MoveMessageToFolder(ctx, msg.SourceMessageID, folder)
		if err != nil {
			moveErrors++
			fmt.Fprintf(os.Stderr, "Warning: move message %d failed: %v\n", msg.ID, err)
			if ctx.Err() != nil {
				moveErrors += len(msgs) - i - 1
				break
			}
		} else if confirmed {
			moved = append(moved, msg.ID)
		} else {
			moveNoops++
		}

		processed := i + 1
		if processed%labelWriteChunkSize == 0 || processed == len(msgs) {
			fmt.Fprintf(os.Stderr,
				"Processed %d/%d move(s): %d confirmed, %d no-op, %d error(s).\n",
				processed, len(msgs), len(moved), moveNoops, moveErrors)
		}
	}
	return moved, moveNoops, moveErrors
}

func init() {
	triageQueueCmd.Flags().StringVar(&triageAccount, "account", "",
		"Account identifier (email or display name); defaults to the only syncable account")
	triageQueueCmd.Flags().StringVar(&triageLabel, "label", "INBOX",
		"Label/folder pool to scan")
	triageQueueCmd.Flags().StringVar(&triageBefore, triageFlagBefore, "",
		"Staleness cutoff date (YYYY-MM-DD, interpreted as UTC midnight); mutually exclusive with --older-than-workdays")
	triageQueueCmd.Flags().IntVar(&triageOlderWorkdays, triageFlagOlderWorkdays, 0,
		"Messages are stale once N full working days (Mon-Fri, midnight boundaries in local time) have passed; mutually exclusive with --before")
	triageQueueCmd.Flags().StringArrayVar(&triageNotLabels, "not-label", nil,
		"Treated label to skip (repeatable, case-insensitive exact match)")
	triageQueueCmd.Flags().StringVar(&triageMoveTo, "move-to", "",
		"Destination folder for stale untreated messages (required)")
	triageQueueCmd.Flags().BoolVar(&triageReportReplied, "report-replied", false,
		"Withhold ANSWERED messages from moving and report them with the latest own reply text")
	triageQueueCmd.Flags().StringVar(&triageSkipIDs, "skip-ids", "",
		"Comma-separated archive message IDs to exclude as treated (from a prior --report-replied review)")
	triageQueueCmd.Flags().BoolVar(&triageDryRun, "dry-run", false,
		"Classify and report without moving anything")
	rootCmd.AddCommand(triageQueueCmd)
}

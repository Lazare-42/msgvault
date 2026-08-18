package cmd

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whatsapp/historymerge"
)

var (
	mergeWhatsAppHistoryFrom               string
	mergeWhatsAppHistoryInto               string
	mergeWhatsAppHistoryApply              bool
	mergeWhatsAppHistoryIdentifier         string
	mergeWhatsAppHistoryMaxAttachmentBytes int64
)

var mergeWhatsAppHistoryCmd = &cobra.Command{
	Use:   "merge-whatsapp-history --from <home> --into <home>",
	Short: "One-time backfill of WhatsApp history from another archive home",
	Long: `Copy WhatsApp-sourced conversations, messages, attachments, and
reactions from one msgvault archive home (--from) into another (--into).

This is a migration/backfill tool, not a general sync feature. It exists for
one situation: a combined archive was split into a Gmail-only home and a
separate, isolated WhatsApp home (because a live WhatsApp bridge must hold an
exclusive lock on its archive), and WhatsApp history captured before the
split is now only visible in the old combined home.

--from is opened read-only and is never modified — safe to run alongside an
active Gmail sync or query against that archive. Only messages belonging to
a 'whatsapp' source in --from are considered; any other sources (Gmail,
IMAP, etc.) mixed into the same archive are ignored.

Every --from WhatsApp source must have a matching WhatsApp source already
registered in --into (matched by identifier, i.e. the WhatsApp JID/account —
not by internal source id, which is independent per archive). This tool
backfills an EXISTING live-synced account; it never creates a new source in
--into. Use --identifier to scope to one account when --from has more than
one WhatsApp source.

Messages are deduplicated by (source, source_message_id) — the same key the
live bridge itself uses — so this command is idempotent: running it twice,
or running it after --into has independently re-synced the same messages,
never creates duplicates. Attachments are deduplicated by content hash;
a blob already present in --into (by hash) is never re-copied.

The default is a dry-run report: every counter describes what WOULD be
copied, and nothing is written. Pass --apply to actually write.

--apply requires exclusive write access to --into, the same lock a live
WhatsApp bridge (whatsapp-live-mcp) or 'msgvault serve' holds for that home
for its entire run. If either is running against --into, --apply fails fast
with an actionable error instead of racing it — stop that process first,
run --apply, then restart it. --from needs no such precaution: it is only
ever read.

After a successful --apply, the analytics cache for --into is rebuilt so
newly-copied history is visible in the TUI/web UI immediately.

Example:
  msgvault merge-whatsapp-history --from ~/old-combined-archive --into ~/whatsapp-archive
  msgvault merge-whatsapp-history --from ~/old-combined-archive --into ~/whatsapp-archive --apply
`,
	Args: cobra.NoArgs,
	RunE: runMergeWhatsAppHistory,
}

func runMergeWhatsAppHistory(cmd *cobra.Command, _ []string) error {
	if mergeWhatsAppHistoryFrom == "" || mergeWhatsAppHistoryInto == "" {
		return usageErr(cmd, errors.New("--from and --into are both required"))
	}

	fromHome, err := filepath.Abs(mergeWhatsAppHistoryFrom)
	if err != nil {
		return fmt.Errorf("resolve --from %q: %w", mergeWhatsAppHistoryFrom, err)
	}
	intoHome, err := filepath.Abs(mergeWhatsAppHistoryInto)
	if err != nil {
		return fmt.Errorf("resolve --into %q: %w", mergeWhatsAppHistoryInto, err)
	}
	if fromHome == intoHome {
		return usageErr(cmd, errors.New("--from and --into must be different archive homes"))
	}

	fromCfg, err := config.Load("", fromHome)
	if err != nil {
		return fmt.Errorf("load --from config: %w", err)
	}
	intoCfg, err := config.Load("", intoHome)
	if err != nil {
		return fmt.Errorf("load --into config: %w", err)
	}

	fromStore, err := store.OpenReadOnly(fromCfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open --from archive: %w", err)
	}
	defer func() { _ = fromStore.Close() }()

	intoStore, releaseIntoLock, err := openMergeTargetStore(intoCfg, mergeWhatsAppHistoryApply)
	if err != nil {
		return err
	}
	defer func() {
		_ = intoStore.Close()
		if releaseIntoLock != nil {
			releaseIntoLock()
		}
	}()

	pairs, err := historymerge.ResolveSourcePairs(fromStore, intoStore, mergeWhatsAppHistoryIdentifier)
	if err != nil {
		return err
	}

	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Finishing current message...")
	defer stop()

	opts := historymerge.Options{
		From:                fromStore,
		Into:                intoStore,
		FromAttachmentsDir:  fromCfg.AttachmentsDir(),
		IntoAttachmentsDir:  intoCfg.AttachmentsDir(),
		Apply:               mergeWhatsAppHistoryApply,
		MaxAttachmentBytes:  mergeWhatsAppHistoryMaxAttachmentBytes,
	}

	out := cmd.OutOrStdout()
	totalFailures := 0
	totalCopied := 0
	for _, pair := range pairs {
		report, err := historymerge.MergeSource(ctx, opts, pair.From.ID, pair.Into.ID)
		if err != nil {
			return fmt.Errorf("merge %s: %w", pair.From.Identifier, err)
		}
		printMergeWhatsAppHistoryReport(out, pair.From.Identifier, mergeWhatsAppHistoryApply, report)
		totalCopied += report.MessagesCopied
		totalFailures += report.MessagesFailed + report.AttachmentsFailed + report.ReactionsFailed
	}

	if !mergeWhatsAppHistoryApply {
		_, _ = fmt.Fprintln(out, "\nDry run: no changes were made. Re-run with --apply to write.")
		if totalFailures > 0 {
			return fmt.Errorf("dry run encountered %d error(s); see messages above", totalFailures)
		}
		return nil
	}

	if err := rebuildCacheForHome(intoCfg.DatabaseDSN(), intoCfg.AnalyticsDir()); err != nil {
		return fmt.Errorf(
			"copy completed (%d message(s) written), but analytics cache refresh failed: %w; "+
				"run `msgvault --home %s build-cache --full-rebuild` to retry",
			totalCopied, err, intoCfg.Data.DataDir,
		)
	}

	if totalFailures > 0 {
		return fmt.Errorf("merge completed with %d error(s); see messages above", totalFailures)
	}
	return nil
}

// openMergeTargetStore opens --into. A dry run only ever needs to read
// --into (to classify what already exists there), so it uses the same
// concurrency-safe read-only mode as --from and never contends with a live
// bridge. --apply needs to write, so it claims the same cross-process
// write-owner lock a direct CLI writer or `whatsapp-live-mcp` would hold
// (see cmd/msgvault/cmd/write_lock.go) — non-blocking, so a live bridge
// already holding it fails this call immediately with an actionable error
// instead of the two processes racing on the database file.
func openMergeTargetStore(intoCfg *config.Config, apply bool) (*store.Store, func(), error) {
	if !apply {
		st, err := store.OpenReadOnly(intoCfg.DatabaseDSN())
		if err != nil {
			return nil, nil, fmt.Errorf("open --into archive: %w", err)
		}
		return st, nil, nil
	}

	if store.IsPostgresURL(intoCfg.DatabaseDSN()) {
		st, err := store.Open(intoCfg.DatabaseDSN())
		if err != nil {
			return nil, nil, fmt.Errorf("open --into archive: %w", err)
		}
		if err := st.InitSchema(); err != nil {
			_ = st.Close()
			return nil, nil, fmt.Errorf("init --into schema: %w", err)
		}
		return st, nil, nil
	}

	lock, err := tryAcquireWriteOwnerLock(intoCfg.Data.DataDir)
	if err != nil {
		var heldErr writeOwnerLockHeldError
		if errors.As(err, &heldErr) {
			return nil, nil, archiveOwnedError(intoCfg.Data.DataDir)
		}
		return nil, nil, fmt.Errorf("acquire --into write lock: %w", err)
	}
	release := func() {
		if cerr := lock.Close(); cerr != nil {
			logger.Warn("release --into write-owner lock", "error", cerr)
		}
	}

	st, err := store.Open(intoCfg.DatabaseDSN())
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("open --into archive: %w", err)
	}
	if err := st.InitSchema(); err != nil {
		_ = st.Close()
		release()
		return nil, nil, fmt.Errorf("init --into schema: %w", err)
	}
	if _, err := st.RunStartupMigrations(intoCfg.Identity.Addresses); err != nil {
		logger.Warn("--into startup migration failed", "error", err)
	}
	return st, release, nil
}

func printMergeWhatsAppHistoryReport(out io.Writer, identifier string, apply bool, r *historymerge.Report) {
	verb := "Would copy"
	if apply {
		verb = "Copied"
	}
	_, _ = fmt.Fprintf(out, "\n=== %s ===\n", identifier)
	_, _ = fmt.Fprintf(out, "%-24s %d\n", "Conversations examined:", r.Conversations)
	_, _ = fmt.Fprintf(out, "%-24s %d\n", "Messages scanned:", r.MessagesScanned)
	_, _ = fmt.Fprintf(out, "%-24s %d\n", "Already in target:", r.MessagesAlreadyInTarget)
	_, _ = fmt.Fprintf(out, "%-24s %d\n", "Messages "+verb+":", r.MessagesCopied)
	if r.MessagesFailed > 0 {
		_, _ = fmt.Fprintf(out, "%-24s %d\n", "Messages failed:", r.MessagesFailed)
	}
	if r.AttachmentsWithContent > 0 || r.AttachmentMarkers > 0 {
		_, _ = fmt.Fprintf(out, "%-24s %d (already in target: %d, %s: %d)\n",
			"Attachments w/ content:", r.AttachmentsWithContent, r.AttachmentsAlreadyStored,
			verb, r.AttachmentsCopied+r.AttachmentsWouldCopy)
		if r.AttachmentMarkers > 0 {
			_, _ = fmt.Fprintf(out, "%-24s %d\n", "Attachment markers:", r.AttachmentMarkers)
		}
		if r.AttachmentsFailed > 0 {
			_, _ = fmt.Fprintf(out, "%-24s %d\n", "Attachments failed:", r.AttachmentsFailed)
		}
	}
	if r.ReactionsScanned > 0 {
		_, _ = fmt.Fprintf(out, "%-24s %d\n", "Reactions "+verb+":", r.ReactionsCopied+r.ReactionsWouldCopy)
		if r.ReactionsFailed > 0 {
			_, _ = fmt.Fprintf(out, "%-24s %d\n", "Reactions failed:", r.ReactionsFailed)
		}
	}
	for _, e := range r.Errors {
		_, _ = fmt.Fprintf(out, "  error: %s\n", e)
	}
}

func init() {
	mergeWhatsAppHistoryCmd.Flags().StringVar(&mergeWhatsAppHistoryFrom, "from", "", "source archive home to read WhatsApp history from (required)")
	mergeWhatsAppHistoryCmd.Flags().StringVar(&mergeWhatsAppHistoryInto, "into", "", "target archive home to backfill WhatsApp history into (required)")
	mergeWhatsAppHistoryCmd.Flags().BoolVar(&mergeWhatsAppHistoryApply, "apply", false, "write the copy (default is a dry-run report only)")
	mergeWhatsAppHistoryCmd.Flags().StringVar(&mergeWhatsAppHistoryIdentifier, "identifier", "", "scope to one WhatsApp source identifier (JID) in --from, when it has more than one")
	mergeWhatsAppHistoryCmd.Flags().Int64Var(&mergeWhatsAppHistoryMaxAttachmentBytes, "max-attachment-bytes", 0, "max size of a single attachment blob to copy (default 300MiB)")
	_ = mergeWhatsAppHistoryCmd.MarkFlagRequired("from")
	_ = mergeWhatsAppHistoryCmd.MarkFlagRequired("into")
	rootCmd.AddCommand(mergeWhatsAppHistoryCmd)
}

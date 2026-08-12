package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
)

var (
	modifyLabelsAccount   string
	modifyLabelsIDs       string
	modifyLabelsSourceIDs string
	modifyLabelsAdd       []string
	modifyLabelsRemove    []string
	modifyLabelsDryRun    bool
)

var modifyLabelsCmd = &cobra.Command{
	Use:   "modify-labels",
	Short: "Add and/or remove labels on messages via the account's mail server",
	Long: `Add and/or remove labels on messages via the account's mail server.

Messages are addressed either by archive message ID (--ids, the numeric
IDs the TUI and query interface show, resolved through the msgvault
daemon) or directly by source message ID (--source-ids, the mail
server's own identifiers). At least one of the two is required, and so
is at least one of --add/--remove.

For IMAP and Microsoft 365 accounts the supported labels are UNREAD,
STARRED, INBOX, "folder:<name>" (MOVE into a mailbox, created on
demand), and "keyword:<name>" (set/clear an IMAP keyword flag; Exchange
surfaces keywords as Outlook categories).

Writes require an IMAP-backed source; --dry-run resolves and reports
without touching the mail server and works for any source type. The
result is a single JSON object on stdout; progress goes to stderr.

Examples:
  msgvault modify-labels --ids 12,34 --add keyword:Handled --remove UNREAD
  msgvault modify-labels --account user@example.com --ids 12 \
    --add "folder:Recruiting,keyword:Handled"
  msgvault modify-labels --source-ids "INBOX|41,INBOX|57" --add STARRED --dry-run`,
	Args: cobra.NoArgs,
	RunE: runModifyLabels,
}

// modifyLabelsReport is the single JSON object emitted on stdout. Modified
// lists the source message IDs of successfully modified messages.
type modifyLabelsReport struct {
	Modified []string `json:"modified"`
	Errors   int      `json:"errors"`
	DryRun   bool     `json:"dry_run"`
}

// modifyLabelsRequest is the validated flag input of a modify-labels run.
type modifyLabelsRequest struct {
	archiveIDs   []int64
	sourceIDs    []string
	addLabels    []string
	removeLabels []string
}

// parseModifyLabelsRequest validates the raw flag values: at least one
// message (via --ids or --source-ids) and at least one label operation
// (via --add or --remove) must be given.
func parseModifyLabelsRequest(ids, sourceIDs string, add, remove []string) (modifyLabelsRequest, error) {
	req := modifyLabelsRequest{
		addLabels:    cleanLabelList(add),
		removeLabels: cleanLabelList(remove),
	}
	if len(req.addLabels) == 0 && len(req.removeLabels) == 0 {
		return req, errors.New("at least one of --add or --remove is required")
	}
	archiveIDs, err := parseArchiveIDList(ids)
	if err != nil {
		return req, err
	}
	req.archiveIDs = archiveIDs
	req.sourceIDs = splitCommaList(sourceIDs)
	if len(req.archiveIDs) == 0 && len(req.sourceIDs) == 0 {
		return req, errors.New("at least one of --ids or --source-ids is required")
	}
	return req, nil
}

// parseArchiveIDList parses the comma-separated --ids value into archive
// message IDs. Empty segments are ignored.
func parseArchiveIDList(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range splitCommaList(raw) {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid --ids entry %q (expected a numeric archive message ID)", part)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// splitCommaList splits a comma-separated value, trimming whitespace and
// dropping empty segments.
func splitCommaList(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// cleanLabelList trims whitespace and drops empty entries from a label
// flag value.
func cleanLabelList(labels []string) []string {
	var out []string
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

func runModifyLabels(cmd *cobra.Command, _ []string) error {
	req, err := parseModifyLabelsRequest(
		modifyLabelsIDs, modifyLabelsSourceIDs, modifyLabelsAdd, modifyLabelsRemove)
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
	account, err := resolveSyncableAccount(accounts, modifyLabelsAccount)
	if err != nil {
		return err
	}

	// Resolve the live mail client before touching anything so
	// configuration problems surface immediately. Dry runs never touch
	// the mail server and skip this entirely.
	var apiClient gmail.API
	if !modifyLabelsDryRun {
		src, err := lookupAccountWriteSource(account)
		if err != nil {
			return err
		}
		if src.SourceType != sourceTypeIMAP {
			return fmt.Errorf(
				"account %s is a %s source; modify-labels writes use the IMAP label primitives "+
					"and support only IMAP-backed sources (use --dry-run to preview without modifying)",
				account.Email, src.SourceType)
		}
		apiClient, err = buildAPIClient(ctx, src, oauthManagerCache(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = apiClient.Close() }()
	}

	resolved, err := resolveArchiveMessageIDs(ctx, engine, account, req.archiveIDs)
	if err != nil {
		return err
	}
	targets := dedupeStrings(append(resolved, req.sourceIDs...))

	report := modifyLabelsReport{Modified: []string{}, DryRun: modifyLabelsDryRun}
	if modifyLabelsDryRun {
		report.Modified = targets
		fmt.Fprintf(os.Stderr, "Dry run: %d message(s) would be modified.\n", len(targets))
		return printJSON(report)
	}

	okIdx, errCount := batchModifyLabelsChunked(
		ctx, apiClient, targets, req.addLabels, req.removeLabels, "Modified")
	for _, i := range okIdx {
		report.Modified = append(report.Modified, targets[i])
	}
	report.Errors = errCount
	fmt.Fprintf(os.Stderr, "Modified %d message(s) (%d error(s)).\n", len(report.Modified), errCount)
	if err := printJSON(report); err != nil {
		return err
	}
	if errCount > 0 {
		return fmt.Errorf("%d message(s) failed to modify", errCount)
	}
	return nil
}

// resolveArchiveMessageIDs resolves archive message IDs to the source
// message IDs the mail server understands, verifying each message exists
// and belongs to the selected account. Any failure aborts the run before
// anything is modified.
func resolveArchiveMessageIDs(
	ctx context.Context,
	engine *daemonclient.Engine,
	account daemonclient.CLIAccount,
	ids []int64,
) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		md, err := engine.GetMessage(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("resolve message %d: %w", id, err)
		}
		if md == nil {
			return nil, fmt.Errorf("message %d not found in the archive", id)
		}
		if md.SourceID != account.ID {
			return nil, fmt.Errorf(
				"message %d belongs to another account (source %d, not %s)", id, md.SourceID, account.Email)
		}
		if md.SourceMessageID == "" {
			return nil, fmt.Errorf("message %d has no source message ID", id)
		}
		out = append(out, md.SourceMessageID)
	}
	return out, nil
}

// dedupeStrings removes duplicates while preserving first-seen order, so
// a message named both by archive ID and source ID is modified once.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func init() {
	modifyLabelsCmd.Flags().StringVar(&modifyLabelsAccount, "account", "",
		"Account identifier (email or display name); defaults to the only syncable account")
	modifyLabelsCmd.Flags().StringVar(&modifyLabelsIDs, "ids", "",
		"Comma-separated archive message IDs (resolved to source message IDs via the daemon)")
	modifyLabelsCmd.Flags().StringVar(&modifyLabelsSourceIDs, "source-ids", "",
		"Comma-separated source message IDs, used as-is")
	modifyLabelsCmd.Flags().StringSliceVar(&modifyLabelsAdd, "add", nil,
		"Labels to add (comma-separated or repeatable), e.g. STARRED,keyword:Handled")
	modifyLabelsCmd.Flags().StringSliceVar(&modifyLabelsRemove, "remove", nil,
		"Labels to remove (comma-separated or repeatable), e.g. UNREAD,keyword:Handled")
	modifyLabelsCmd.Flags().BoolVar(&modifyLabelsDryRun, "dry-run", false,
		"Resolve and report without modifying anything")
	rootCmd.AddCommand(modifyLabelsCmd)
}

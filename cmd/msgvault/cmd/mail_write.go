package cmd

// Shared plumbing for commands that write back to a live mail server
// (triage-queue, modify-labels): resolving the target daemon account,
// looking up the local store.Source row that carries its credentials,
// and executing chunked BatchModifyLabels calls.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

// labelWriteChunkSize bounds each BatchModifyLabels call so one bad
// message cannot fail an arbitrarily large batch.
const labelWriteChunkSize = 40

// resolveSyncableAccount picks the target account among the daemon's
// syncable (gmail/imap) accounts: an explicit identifier must match
// exactly one account by email or display name (case-insensitive);
// with no identifier the single syncable account is used.
func resolveSyncableAccount(accounts []daemonclient.CLIAccount, input string) (daemonclient.CLIAccount, error) {
	var syncable []daemonclient.CLIAccount
	for _, a := range accounts {
		if a.Type == sourceTypeGmail || a.Type == sourceTypeIMAP {
			syncable = append(syncable, a)
		}
	}

	if input == "" {
		switch len(syncable) {
		case 0:
			return daemonclient.CLIAccount{}, errors.New(
				"no syncable (gmail/imap) accounts configured - run 'add-imap' or 'add-account' first")
		case 1:
			return syncable[0], nil
		default:
			return daemonclient.CLIAccount{}, fmt.Errorf(
				"multiple syncable accounts configured (%s) - use --account to pick one",
				strings.Join(syncableAccountNames(syncable), ", "))
		}
	}

	var matches []daemonclient.CLIAccount
	for _, a := range syncable {
		if strings.EqualFold(a.Email, input) ||
			(a.DisplayName != "" && strings.EqualFold(a.DisplayName, input)) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return daemonclient.CLIAccount{}, fmt.Errorf(
			"no syncable account found for %q (try 'msgvault list-accounts')", input)
	case 1:
		return matches[0], nil
	default:
		return daemonclient.CLIAccount{}, fmt.Errorf(
			"ambiguous account %q matches multiple sources: %s",
			input, strings.Join(syncableAccountNames(matches), ", "))
	}
}

func syncableAccountNames(accounts []daemonclient.CLIAccount) []string {
	names := make([]string, 0, len(accounts))
	for _, a := range accounts {
		names = append(names, fmt.Sprintf("%s (%s)", a.Email, a.Type))
	}
	sort.Strings(names)
	return names
}

// lookupAccountWriteSource resolves the local store.Source row backing the
// daemon account, which carries the sync_config/credentials needed to
// build a live mail client. The archive is opened read-only, so this is
// safe alongside the running daemon; in remote daemon mode the local
// archive does not exist and mail-server writes are unavailable.
func lookupAccountWriteSource(account daemonclient.CLIAccount) (*store.Source, error) {
	s, err := store.OpenReadOnly(cfg.DatabaseDSN())
	if err != nil {
		return nil, fmt.Errorf(
			"open local archive to load source credentials (mail-server writes require running where the account was added): %w", err)
	}
	defer func() { _ = s.Close() }()

	return lookupAccountWriteSourceIn(s, account)
}

// lookupAccountWriteSourceIn is the store-injectable core of
// lookupAccountWriteSource. IMAP source identifiers are
// imaps://user@host:port URLs with the account email stored in
// display_name, so the lookup must match either column — the same
// resolver sync/sync-full use.
func lookupAccountWriteSourceIn(s *store.Store, account daemonclient.CLIAccount) (*store.Source, error) {
	sources, err := s.GetSourcesByIdentifierOrDisplayName(account.Email)
	if err != nil {
		return nil, fmt.Errorf("look up source for %s: %w", account.Email, err)
	}
	return pickAccountWriteSource(sources, account)
}

// pickAccountWriteSource selects, among the local rows matching the
// account email by identifier or display name, the one backing the
// daemon account (same source type). Exactly one row must match.
func pickAccountWriteSource(sources []*store.Source, account daemonclient.CLIAccount) (*store.Source, error) {
	var matches []*store.Source
	for _, src := range sources {
		if src.SourceType == account.Type {
			matches = append(matches, src)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no local %s source found for %s", account.Type, account.Email)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, src := range matches {
			names = append(names, src.Identifier)
		}
		sort.Strings(names)
		return nil, fmt.Errorf(
			"multiple local %s sources match %s (%s); remove or rename the duplicate before writing",
			account.Type, account.Email, strings.Join(names, ", "))
	}
}

// batchModifyLabelsChunked applies the add/remove label operations to
// sourceIDs in chunks of labelWriteChunkSize. A failed chunk counts all
// of its messages as errors and the run continues; when the context is
// cancelled the remaining messages are counted as errors and the run
// stops. Returns the indices into sourceIDs of successfully modified
// messages; progressVerb (e.g. "Moved") labels the stderr progress lines.
func batchModifyLabelsChunked(
	ctx context.Context,
	client gmail.API,
	sourceIDs, addLabels, removeLabels []string,
	progressVerb string,
) (okIdx []int, errCount int) {
	for start := 0; start < len(sourceIDs); start += labelWriteChunkSize {
		end := min(start+labelWriteChunkSize, len(sourceIDs))
		chunk := sourceIDs[start:end]
		if err := client.BatchModifyLabels(ctx, chunk, addLabels, removeLabels); err != nil {
			errCount += len(chunk)
			fmt.Fprintf(os.Stderr, "Warning: label chunk of %d message(s) failed: %v\n", len(chunk), err)
			if ctx.Err() != nil {
				// The remaining chunks would fail the same way; count
				// them as errors and stop.
				errCount += len(sourceIDs) - end
				return okIdx, errCount
			}
			continue
		}
		for i := start; i < end; i++ {
			okIdx = append(okIdx, i)
		}
		fmt.Fprintf(os.Stderr, "%s %d/%d message(s)...\n", progressVerb, len(okIdx), len(sourceIDs))
	}
	return okIdx, errCount
}

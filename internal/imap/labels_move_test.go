package imap

import (
	"context"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
)

func TestClientModifyMessageLabelsMoveToFolder(t *testing.T) {
	client := newDraftTestClient(t)
	id := appendTestMessage(t, client, "INBOX", &gmailapi.DraftCompose{
		To:      []string{"applicant@example.com"},
		Subject: "file me",
		Body:    "body",
	}, nil)

	// "Recruiting" is not pre-created — the move must create it on demand.
	require.NoError(t, client.ModifyMessageLabels(
		context.Background(),
		id,
		[]string{"folder:Recruiting"},
		nil,
	))

	// Gone from INBOX...
	_, err := client.GetMessageRaw(context.Background(), id)
	assert.Error(t, err)

	// ...and present in Recruiting.
	movedIDs := listMailboxMessageIDs(t, client, "Recruiting")
	require.Len(t, movedIDs, 1)
	raw, err := client.GetMessageRaw(context.Background(), movedIDs[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw.Raw), "Subject: file me")
}

func TestClientModifyMessageLabelsMoveToExistingFolderWithFlag(t *testing.T) {
	client := newDraftTestClient(t)
	id := appendTestMessage(t, client, "INBOX", &gmailapi.DraftCompose{
		To:      []string{"applicant@example.com"},
		Subject: "star and file",
		Body:    "body",
	}, nil)

	// Archive pre-exists (created by the harness); a flag rides along with the move.
	require.NoError(t, client.ModifyMessageLabels(
		context.Background(),
		id,
		[]string{"STARRED", "folder:Archive"},
		nil,
	))

	moved := listMailboxMessageIDs(t, client, "Archive")
	require.Len(t, moved, 1)
	_, uid, err := parseCompositeID(moved[0])
	require.NoError(t, err)
	flags := fetchMessageFlags(t, client, "Archive", uid)
	assert.Contains(t, flags, goimap.FlagFlagged)
}

func TestClientModifyMessageLabelsMoveIdempotentWhenAlreadyThere(t *testing.T) {
	client := newDraftTestClient(t)
	id := appendTestMessage(t, client, "Archive", &gmailapi.DraftCompose{
		To:      []string{"a@example.com"},
		Subject: "already here",
		Body:    "body",
	}, nil)

	// Moving to the mailbox it already lives in is a no-op, not an error.
	require.NoError(t, client.ModifyMessageLabels(
		context.Background(),
		id,
		[]string{"folder:Archive"},
		nil,
	))
	assert.Len(t, listMailboxMessageIDs(t, client, "Archive"), 1)
}

func TestParseIMAPLabelOpsFolder(t *testing.T) {
	t.Run("parses folder destination, not upper-cased", func(t *testing.T) {
		ops, err := parseIMAPLabelOps([]string{"folder:Recruiting/2026"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "Recruiting/2026", ops.destFolder)
	})

	t.Run("prefix is case-insensitive", func(t *testing.T) {
		ops, err := parseIMAPLabelOps([]string{"FOLDER:Payroll"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "Payroll", ops.destFolder)
	})

	t.Run("empty destination is rejected", func(t *testing.T) {
		_, err := parseIMAPLabelOps([]string{"folder:   "}, nil)
		require.ErrorContains(t, err, "empty destination")
	})

	t.Run("two different folders conflict", func(t *testing.T) {
		_, err := parseIMAPLabelOps([]string{"folder:A", "folder:B"}, nil)
		require.ErrorContains(t, err, "two folders")
	})

	t.Run("same folder twice is fine", func(t *testing.T) {
		ops, err := parseIMAPLabelOps([]string{"folder:A", "folder:a"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "A", ops.destFolder)
	})

	t.Run("folder move conflicts with archive", func(t *testing.T) {
		_, err := parseIMAPLabelOps([]string{"folder:A"}, []string{"INBOX"})
		require.ErrorContains(t, err, "conflicting IMAP moves")
	})

	t.Run("folder move conflicts with move-to-inbox", func(t *testing.T) {
		_, err := parseIMAPLabelOps([]string{"folder:A", "INBOX"}, nil)
		require.ErrorContains(t, err, "conflicting IMAP moves")
	})

	t.Run("folder can combine with a flag", func(t *testing.T) {
		ops, err := parseIMAPLabelOps([]string{"folder:A", "STARRED"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "A", ops.destFolder)
		assert.Contains(t, ops.addFlags, goimap.FlagFlagged)
	})

	t.Run("removing a folder label is rejected", func(t *testing.T) {
		_, err := parseIMAPLabelOps(nil, []string{"folder:A"})
		require.ErrorContains(t, err, "not supported")
	})
}

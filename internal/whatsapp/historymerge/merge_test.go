package historymerge_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whatsapp/historymerge"
)

// This test builds two real, file-backed SQLite archives through the same
// Store API the live WhatsApp bridge and TUI use (UpsertMessage,
// EnsureParticipantByIdentifier, ReplaceMessageWhatsAppAttachments, ...),
// then drives historymerge.MergeSource — the actual merge-whatsapp-history
// engine — against them. No mocks: the source archive is reopened
// store.OpenReadOnly for every merge call, exactly as the CLI command does,
// so a bug that tried to write to --from would surface as a real SQLite
// "readonly database" error, not a silently-accepted call on a mock.

const (
	fromJID    = "15551234567@s.whatsapp.net" // shared identifier: the account owner
	contactJID = "15557654321@s.whatsapp.net" // a direct-chat contact
	groupJID   = "15559999999-1600000000@g.us"

	waMsgHello   = "wa-hello"   // from only
	waMsgReply   = "wa-reply"   // from only, is_from_me
	waMsgShared  = "wa-shared"  // in both from and into, must dedup
	waMsgGroup   = "wa-group"   // from only, group chat, marker attachment
	waMsgDeleted = "wa-deleted" // from only, soft-deleted, must be skipped
)

type fixture struct {
	fromDBPath   string
	fromAttDir   string
	intoStore    *store.Store
	intoDBPath   string
	intoAttDir   string
	fromSourceID int64
	intoSourceID int64

	imageBytes []byte
	imageHash  string
}

func buildFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()

	fromSt, fromDBPath := newFileStore(t, filepath.Join(base, "from"))
	fromAttDir := filepath.Join(base, "from", "attachments")
	require.NoError(t, os.MkdirAll(fromAttDir, 0o700))

	fromSource, err := fromSt.GetOrCreateSource(store.WhatsAppSourceType, fromJID)
	require.NoError(t, err)

	directConvID, err := fromSt.EnsureConversationWithType(fromSource.ID, contactJID, "direct_chat", "")
	require.NoError(t, err)
	groupConvID, err := fromSt.EnsureConversationWithType(fromSource.ID, groupJID, "group_chat", "Test Group")
	require.NoError(t, err)

	// msg1: inbound from the contact, with a real attachment and both an
	// active and a since-removed reaction.
	msg1 := seedWhatsAppMessage(t, fromSt, fromSource.ID, directConvID, contactJID, waMsgHello,
		contactJID, "Contact", "hello there", false, time.Unix(1700000000, 0).UTC())
	imageBytes := []byte("fake-jpeg-bytes-for-test-fixture")
	imageHash := seedWhatsAppAttachment(t, fromSt, fromAttDir, msg1, contactJID, waMsgHello, imageBytes, "photo.jpg", "image/jpeg")

	selfID, err := fromSt.EnsureParticipantByIdentifier(store.WhatsAppIdentifierType, fromJID, "")
	require.NoError(t, err)
	require.NoError(t, fromSt.SetReaction(msg1, selfID, "emoji", "\U0001F44D", time.Unix(1700000100, 0).UTC()))
	contactID, err := fromSt.EnsureParticipantByIdentifier(store.WhatsAppIdentifierType, contactJID, "Contact")
	require.NoError(t, err)
	require.NoError(t, fromSt.SetReaction(msg1, contactID, "emoji", "\U0001F602", time.Unix(1700000200, 0).UTC()))
	// Retract the contact's reaction — must NOT be replayed by the merge.
	require.NoError(t, fromSt.SetReaction(msg1, contactID, "emoji", "", time.Unix(1700000300, 0).UTC()))

	// msg2: outbound (is_from_me), no attachment.
	seedWhatsAppMessage(t, fromSt, fromSource.ID, directConvID, contactJID, waMsgReply,
		fromJID, "", "hi back", true, time.Unix(1700000400, 0).UTC())

	// msg3: exists in both archives already (simulates independent live
	// sync of the same message after the archives were split).
	seedWhatsAppMessage(t, fromSt, fromSource.ID, directConvID, contactJID, waMsgShared,
		contactJID, "Contact", "already synced independently", false, time.Unix(1700000500, 0).UTC())

	// msg4: group message with a download-failure marker attachment.
	msg4 := seedWhatsAppMessage(t, fromSt, fromSource.ID, groupConvID, groupJID, waMsgGroup,
		contactJID, "Contact", "group message", false, time.Unix(1700000600, 0).UTC())
	seedWhatsAppAttachmentMarker(t, fromSt, msg4, groupJID, waMsgGroup)

	// msg5: soft-deleted — must never be scanned or copied.
	deletedID := seedWhatsAppMessage(t, fromSt, fromSource.ID, directConvID, contactJID, waMsgDeleted,
		contactJID, "Contact", "should not be copied", false, time.Unix(1700000700, 0).UTC())
	_, err = fromSt.DB().Exec(fromSt.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`),
		time.Now().UTC(), deletedID)
	require.NoError(t, err)

	require.NoError(t, fromSt.Close())

	// --- into archive: the matching WhatsApp source already exists
	// (this tool only backfills an existing live-synced account) and has
	// independently live-synced msg3 already, under its own row id.
	intoSt, intoDBPath := newFileStore(t, filepath.Join(base, "into"))
	intoAttDir := filepath.Join(base, "into", "attachments")
	require.NoError(t, os.MkdirAll(intoAttDir, 0o700))

	intoSource, err := intoSt.GetOrCreateSource(store.WhatsAppSourceType, fromJID)
	require.NoError(t, err)

	// Decoy conversation/messages in an unrelated chat, purely to shift
	// into's messages id sequence away from from's, so the test cannot
	// pass by accident if the merge engine ever compared raw row ids
	// instead of source_message_id.
	decoyConvID, err := intoSt.EnsureConversationWithType(intoSource.ID, "15550000000@s.whatsapp.net", "direct_chat", "")
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		seedWhatsAppMessage(t, intoSt, intoSource.ID, decoyConvID, "15550000000@s.whatsapp.net",
			fmt.Sprintf("decoy-%d", i), "15550000000@s.whatsapp.net", "", "decoy", false,
			time.Unix(1600000000+int64(i), 0).UTC())
	}

	intoDirectConvID, err := intoSt.EnsureConversationWithType(intoSource.ID, contactJID, "direct_chat", "")
	require.NoError(t, err)
	intoSharedMsgID := seedWhatsAppMessage(t, intoSt, intoSource.ID, intoDirectConvID, contactJID, waMsgShared,
		contactJID, "Contact", "already synced independently", false, time.Unix(1700000500, 0).UTC())

	// The same image bytes as msg1's attachment are already stored in
	// into (attached to the pre-existing shared message), so the merge
	// should recognize the content by hash and not need to copy bytes.
	preStoredHash := seedWhatsAppAttachment(t, intoSt, intoAttDir, intoSharedMsgID, contactJID, waMsgShared, imageBytes, "photo.jpg", "image/jpeg")
	require.Equal(t, imageHash, preStoredHash, "fixture bug: pre-stored blob must hash the same as msg1's attachment")

	return &fixture{
		fromDBPath: fromDBPath, fromAttDir: fromAttDir,
		intoStore: intoSt, intoDBPath: intoDBPath, intoAttDir: intoAttDir,
		fromSourceID: fromSource.ID, intoSourceID: intoSource.ID,
		imageBytes: imageBytes, imageHash: imageHash,
	}
}

func newFileStore(t *testing.T, dir string) (*store.Store, string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	dbPath := filepath.Join(dir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	return st, dbPath
}

func seedWhatsAppMessage(
	t *testing.T, st *store.Store, sourceID, convID int64,
	chatJID, waMsgID, senderJID, senderName, body string, isFromMe bool, sentAt time.Time,
) int64 {
	t.Helper()
	var senderID sql.NullInt64
	if senderJID != "" {
		pid, err := st.EnsureParticipantByIdentifier(store.WhatsAppIdentifierType, senderJID, senderName)
		require.NoError(t, err)
		senderID = sql.NullInt64{Int64: pid, Valid: true}
		require.NoError(t, st.EnsureConversationParticipant(convID, pid, "member"))
	}
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        sourceID,
		SourceMessageID: store.WhatsAppSourceMessageID(chatJID, waMsgID),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
		ReceivedAt:      sql.NullTime{Time: sentAt, Valid: true},
		InternalDate:    sql.NullTime{Time: sentAt, Valid: true},
		SenderID:        senderID,
		IsFromMe:        isFromMe,
		Snippet:         sql.NullString{String: body, Valid: body != ""},
		SizeEstimate:    int64(len(body)),
	})
	require.NoError(t, err)
	if body != "" {
		require.NoError(t, st.UpsertMessageBody(msgID, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	}
	raw, err := json.Marshal(map[string]string{"chat_jid": chatJID, "message_id": waMsgID, "text": body})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRawWithFormat(msgID, raw, store.WhatsAppRawFormat))
	return msgID
}

func seedWhatsAppAttachment(
	t *testing.T, st *store.Store, attachmentsDir string, msgID int64,
	chatJID, waMsgID string, content []byte, filename, mimeType string,
) string {
	t.Helper()
	att := &mime.Attachment{Filename: filename, ContentType: mimeType, Content: content}
	storagePath, err := export.StoreAttachmentFile(attachmentsDir, att)
	require.NoError(t, err)
	ref := store.AttachmentRef{
		Filename:           filename,
		MimeType:           mimeType,
		MediaType:          "image",
		StoragePath:        storagePath,
		ContentHash:        att.ContentHash,
		Size:               len(content),
		SourceAttachmentID: "whatsapp:" + store.WhatsAppSourceMessageID(chatJID, waMsgID),
	}
	require.NoError(t, st.ReplaceMessageWhatsAppAttachments(msgID, []store.AttachmentRef{ref}))
	require.NoError(t, st.RecomputeMessageAttachmentStats(msgID))
	return att.ContentHash
}

func seedWhatsAppAttachmentMarker(t *testing.T, st *store.Store, msgID int64, chatJID, waMsgID string) {
	t.Helper()
	sourceAttachmentID := "whatsapp:" + store.WhatsAppSourceMessageID(chatJID, waMsgID)
	ref := store.AttachmentRef{
		SourceAttachmentID: sourceAttachmentID,
		StoragePath:        "whatsapp:pending:" + store.WhatsAppSourceMessageID(chatJID, waMsgID),
	}
	require.NoError(t, st.ReplaceMessageWhatsAppAttachments(msgID, []store.AttachmentRef{ref}))
	require.NoError(t, st.RecomputeMessageAttachmentStats(msgID))
}

func messageCount(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), sourceID,
	).Scan(&n))
	return n
}

func TestMergeSource(t *testing.T) {
	f := buildFixture(t)
	defer func() { _ = f.intoStore.Close() }()

	baselineIntoCount := messageCount(t, f.intoStore, f.intoSourceID)
	require.Equal(t, 6, baselineIntoCount, "fixture sanity: 5 decoys + 1 pre-synced shared message")

	// --- Phase 1: dry run. --into is opened read-only, matching exactly
	// what the CLI does when --apply is not passed — any accidental write
	// attempt would surface as a real SQLite readonly-database error,
	// recorded in report.Errors, not a silent no-op.
	intoRO, err := store.OpenReadOnly(f.intoDBPath)
	require.NoError(t, err)
	fromRO, err := store.OpenReadOnly(f.fromDBPath)
	require.NoError(t, err)

	dryOpts := historymerge.Options{
		From: fromRO, Into: intoRO,
		FromAttachmentsDir: f.fromAttDir, IntoAttachmentsDir: f.intoAttDir,
		Apply: false,
	}
	dryReport, err := historymerge.MergeSource(context.Background(), dryOpts, f.fromSourceID, f.intoSourceID)
	require.NoError(t, err)
	require.Empty(t, dryReport.Errors)

	assert.Equal(t, 4, dryReport.MessagesScanned, "wa-hello, wa-reply, wa-shared, wa-group — wa-deleted must be excluded")
	assert.Equal(t, 1, dryReport.MessagesAlreadyInTarget, "wa-shared already exists in into")
	assert.Equal(t, 3, dryReport.MessagesCopied, "wa-hello, wa-reply, wa-group would be copied")
	assert.Equal(t, 0, dryReport.MessagesFailed)
	assert.Equal(t, 1, dryReport.AttachmentsWithContent, "only wa-hello has real attachment content")
	assert.Equal(t, 1, dryReport.AttachmentsAlreadyStored, "same bytes already stored in into by hash")
	assert.Equal(t, 0, dryReport.AttachmentsWouldCopy, "already-stored blob does not need copying")
	assert.Equal(t, 1, dryReport.AttachmentMarkers, "wa-group's failed-download marker")
	assert.Equal(t, 1, dryReport.ReactionsWouldCopy, "only the active reaction, not the retracted one")

	require.NoError(t, intoRO.Close())
	require.NoError(t, fromRO.Close())

	// into must be completely unchanged after a dry run.
	assert.Equal(t, baselineIntoCount, messageCount(t, f.intoStore, f.intoSourceID))

	// --- Phase 2: apply.
	fromRO2, err := store.OpenReadOnly(f.fromDBPath)
	require.NoError(t, err)
	defer func() { _ = fromRO2.Close() }()

	applyOpts := historymerge.Options{
		From: fromRO2, Into: f.intoStore,
		FromAttachmentsDir: f.fromAttDir, IntoAttachmentsDir: f.intoAttDir,
		Apply: true,
	}
	applyReport, err := historymerge.MergeSource(context.Background(), applyOpts, f.fromSourceID, f.intoSourceID)
	require.NoError(t, err)
	require.Empty(t, applyReport.Errors)

	assert.Equal(t, 4, applyReport.MessagesScanned)
	assert.Equal(t, 1, applyReport.MessagesAlreadyInTarget)
	assert.Equal(t, 3, applyReport.MessagesCopied)
	assert.Equal(t, 1, applyReport.AttachmentsAlreadyStored)
	assert.Equal(t, 0, applyReport.AttachmentsCopied, "the only real attachment blob was already present by hash")
	assert.Equal(t, 1, applyReport.AttachmentMarkers)
	assert.Equal(t, 1, applyReport.ReactionsCopied)

	assert.Equal(t, baselineIntoCount+3, messageCount(t, f.intoStore, f.intoSourceID))

	// Table of per-message expectations, verified against the target
	// archive's own row shape — not the report counters above.
	cases := []struct {
		name             string
		sourceMessageID  string
		wantConvJID      string
		wantConvType     string
		wantBody         string
		wantAttachHash   string // "" = no attachment expected
		wantAttachMarker bool
		wantReactions    int
	}{
		{
			name:            "wa-hello copied with body, attachment, and active reaction",
			sourceMessageID: store.WhatsAppSourceMessageID(contactJID, waMsgHello),
			wantConvJID:     contactJID, wantConvType: "direct_chat",
			wantBody: "hello there", wantAttachHash: f.imageHash, wantReactions: 1,
		},
		{
			name:            "wa-reply copied (outbound, no attachment)",
			sourceMessageID: store.WhatsAppSourceMessageID(contactJID, waMsgReply),
			wantConvJID:     contactJID, wantConvType: "direct_chat",
			wantBody: "hi back",
		},
		{
			name:            "wa-group copied with marker attachment, correct conversation type/title",
			sourceMessageID: store.WhatsAppSourceMessageID(groupJID, waMsgGroup),
			wantConvJID:     groupJID, wantConvType: "group_chat",
			wantBody: "group message", wantAttachMarker: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgID, err := f.intoStore.GetWhatsAppMessageIDBySource(context.Background(), f.intoSourceID, tc.sourceMessageID)
			require.NoError(t, err)
			require.NotZero(t, msgID, "message must exist in target")

			var convID int64
			require.NoError(t, f.intoStore.DB().QueryRow(
				f.intoStore.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), msgID,
			).Scan(&convID))

			var gotConvJID, gotConvType string
			require.NoError(t, f.intoStore.DB().QueryRow(
				f.intoStore.Rebind(`SELECT source_conversation_id, conversation_type FROM conversations WHERE id = ? AND source_id = ?`),
				convID, f.intoSourceID,
			).Scan(&gotConvJID, &gotConvType), "conversation_id must point at a real conversation row in the target")
			assert.Equal(t, tc.wantConvJID, gotConvJID)
			assert.Equal(t, tc.wantConvType, gotConvType)

			bodyText, _, found, err := f.intoStore.GetMessageBodyText(msgID)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, tc.wantBody, bodyText.String)

			refs, err := f.intoStore.MessageWhatsAppAttachments(msgID)
			require.NoError(t, err)
			switch {
			case tc.wantAttachHash != "":
				require.Len(t, refs, 1)
				for _, ref := range refs {
					assert.Equal(t, tc.wantAttachHash, ref.ContentHash)
					// Attachment content must actually be reachable at the
					// canonical content-addressed path under into's
					// attachments dir — not just referenced in the DB.
					data, err := os.ReadFile(filepath.Join(f.intoAttDir, ref.StoragePath))
					require.NoError(t, err, "attachment blob must be reachable on disk")
					assert.Equal(t, f.imageBytes, data)
				}
			case tc.wantAttachMarker:
				require.Len(t, refs, 1)
				for _, ref := range refs {
					assert.Empty(t, ref.ContentHash)
					assert.Contains(t, ref.StoragePath, "pending")
				}
			default:
				assert.Empty(t, refs)
			}

			reactions, err := f.intoStore.ListActiveWhatsAppReactions(msgID)
			require.NoError(t, err)
			assert.Len(t, reactions, tc.wantReactions)
		})
	}

	// wa-shared must still resolve to the ORIGINAL into-side row — the
	// merge must not have created a second copy under a new id.
	sharedID, err := f.intoStore.GetWhatsAppMessageIDBySource(context.Background(), f.intoSourceID, store.WhatsAppSourceMessageID(contactJID, waMsgShared))
	require.NoError(t, err)
	require.NotZero(t, sharedID)

	// wa-deleted must never have been created in the target.
	deletedTargetID, err := f.intoStore.GetWhatsAppMessageIDBySource(context.Background(), f.intoSourceID, store.WhatsAppSourceMessageID(contactJID, waMsgDeleted))
	require.NoError(t, err)
	assert.Zero(t, deletedTargetID, "soft-deleted source message must not be copied")

	// --- Phase 3: idempotency. Running --apply again must not duplicate
	// anything, and must report everything as already present.
	fromRO3, err := store.OpenReadOnly(f.fromDBPath)
	require.NoError(t, err)
	defer func() { _ = fromRO3.Close() }()

	rerunOpts := historymerge.Options{
		From: fromRO3, Into: f.intoStore,
		FromAttachmentsDir: f.fromAttDir, IntoAttachmentsDir: f.intoAttDir,
		Apply: true,
	}
	rerunReport, err := historymerge.MergeSource(context.Background(), rerunOpts, f.fromSourceID, f.intoSourceID)
	require.NoError(t, err)
	require.Empty(t, rerunReport.Errors)

	assert.Equal(t, 4, rerunReport.MessagesScanned)
	assert.Equal(t, 4, rerunReport.MessagesAlreadyInTarget, "all four now already exist in target")
	assert.Equal(t, 0, rerunReport.MessagesCopied)
	assert.Equal(t, baselineIntoCount+3, messageCount(t, f.intoStore, f.intoSourceID), "re-running must not duplicate any message")
}

func TestResolveSourcePairsRequiresExistingTargetSource(t *testing.T) {
	base := t.TempDir()
	fromSt, _ := newFileStore(t, filepath.Join(base, "from"))
	defer func() { _ = fromSt.Close() }()
	intoSt, _ := newFileStore(t, filepath.Join(base, "into"))
	defer func() { _ = intoSt.Close() }()

	_, err := fromSt.GetOrCreateSource(store.WhatsAppSourceType, fromJID)
	require.NoError(t, err)
	// into has no whatsapp source at all yet.

	_, err = historymerge.ResolveSourcePairs(fromSt, intoSt, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), fromJID)
}

func TestResolveSourcePairsMatchesByIdentifier(t *testing.T) {
	base := t.TempDir()
	fromSt, _ := newFileStore(t, filepath.Join(base, "from"))
	defer func() { _ = fromSt.Close() }()
	intoSt, _ := newFileStore(t, filepath.Join(base, "into"))
	defer func() { _ = intoSt.Close() }()

	fromSource, err := fromSt.GetOrCreateSource(store.WhatsAppSourceType, fromJID)
	require.NoError(t, err)
	intoSource, err := intoSt.GetOrCreateSource(store.WhatsAppSourceType, fromJID)
	require.NoError(t, err)

	pairs, err := historymerge.ResolveSourcePairs(fromSt, intoSt, "")
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	assert.Equal(t, fromSource.ID, pairs[0].From.ID)
	assert.Equal(t, intoSource.ID, pairs[0].Into.ID)
}

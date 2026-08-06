package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWhatsAppOutboxLifecycle(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	requirepkg.NoError(t, err)
	convID, err := st.EnsureConversationWithType(source.ID, "15557654321@s.whatsapp.net", "direct_chat", "")
	requirepkg.NoError(t, err)

	id, created, err := st.InsertWhatsAppOutboxIfAbsent(ctx, store.WhatsAppOutboxInsert{
		LocalRequestID: "req-1",
		SourceID:       source.ID,
		ConversationID: sql.NullInt64{Int64: convID, Valid: true},
		Kind:           store.WhatsAppOutboxMessage,
		ChatJID:        "15557654321@s.whatsapp.net",
		Body:           sql.NullString{String: "hello", Valid: true},
	})
	requirepkg.NoError(t, err)
	requirepkg.NotZero(t, id)
	assertpkg.True(t, created)

	dupID, dupCreated, err := st.InsertWhatsAppOutboxIfAbsent(ctx, store.WhatsAppOutboxInsert{
		LocalRequestID: "req-1",
		SourceID:       source.ID,
		Kind:           store.WhatsAppOutboxMessage,
		ChatJID:        "15557654321@s.whatsapp.net",
		Body:           sql.NullString{String: "ignored", Valid: true},
	})
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, id, dupID)
	assertpkg.False(t, dupCreated)

	requirepkg.NoError(t, st.MarkWhatsAppOutboxSending(ctx, id))
	rec, err := st.GetWhatsAppOutbox(ctx, id)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, store.WhatsAppOutboxSending, rec.Status)
	assertpkg.True(t, rec.CreatedAt.Before(rec.UpdatedAt) || rec.CreatedAt.Equal(rec.UpdatedAt))

	msgID, err := st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: store.WhatsAppSourceMessageID("15557654321@s.whatsapp.net", "remote-1"),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: time.Unix(10, 0), Valid: true},
	})
	requirepkg.NoError(t, err)

	requirepkg.NoError(t, st.MarkWhatsAppOutboxSent(ctx, id, "remote-1", msgID))
	rec, err = st.GetWhatsAppOutbox(ctx, id)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, store.WhatsAppOutboxSent, rec.Status)
	assertpkg.Equal(t, "remote-1", rec.RemoteMessageID.String)
	assertpkg.Equal(t, msgID, rec.MessageID.Int64)
}

func TestSetReactionReplaceAndClear(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	requirepkg.NoError(t, err)
	convID, err := st.EnsureConversationWithType(source.ID, "15557654321@s.whatsapp.net", "direct_chat", "")
	requirepkg.NoError(t, err)
	participantID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15557654321@s.whatsapp.net", "")
	requirepkg.NoError(t, err)
	msgID, err := st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: store.WhatsAppSourceMessageID("15557654321@s.whatsapp.net", "msg-1"),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: time.Unix(10, 0), Valid: true},
	})
	requirepkg.NoError(t, err)

	requirepkg.NoError(t, st.SetReaction(msgID, participantID, "emoji", "👍", time.Unix(20, 0)))
	requirepkg.NoError(t, st.SetReaction(msgID, participantID, "emoji", "❤️", time.Unix(30, 0)))

	var activeValue string
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT reaction_value FROM reactions
		WHERE message_id = ? AND participant_id = ? AND removed_at IS NULL
	`), msgID, participantID).Scan(&activeValue))
	assertpkg.Equal(t, "❤️", activeValue)

	var removedCount int
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM reactions
		WHERE message_id = ? AND participant_id = ? AND reaction_value = ? AND removed_at IS NOT NULL
	`), msgID, participantID, "👍").Scan(&removedCount))
	assertpkg.Equal(t, 1, removedCount)

	requirepkg.NoError(t, st.SetReaction(msgID, participantID, "emoji", "", time.Unix(40, 0)))
	var activeCount int
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM reactions
		WHERE message_id = ? AND participant_id = ? AND removed_at IS NULL
	`), msgID, participantID).Scan(&activeCount))
	assertpkg.Equal(t, 0, activeCount)
}

func TestGetWhatsAppMessageRef(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	requirepkg.NoError(t, err)
	convID, err := st.EnsureConversationWithType(source.ID, "120363@g.us", "group_chat", "")
	requirepkg.NoError(t, err)
	participantID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15557654321@s.whatsapp.net", "")
	requirepkg.NoError(t, err)

	sourceMessageID := store.WhatsAppSourceMessageID("120363@g.us", "ABCDEF")
	msgID, err := st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: sourceMessageID,
		MessageType:     store.WhatsAppMessageType,
		SenderID:        sql.NullInt64{Int64: participantID, Valid: true},
		SentAt:          sql.NullTime{Time: time.Unix(10, 0), Valid: true},
	})
	requirepkg.NoError(t, err)

	gotID, err := st.GetWhatsAppMessageIDBySource(ctx, source.ID, sourceMessageID)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, msgID, gotID)

	ref, err := st.GetWhatsAppMessageRef(ctx, msgID)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, msgID, ref.ID)
	assertpkg.Equal(t, "120363@g.us", ref.ChatJID)
	assertpkg.Equal(t, "ABCDEF", ref.RemoteMessageID)
	assertpkg.Equal(t, "15557654321@s.whatsapp.net", ref.SenderJID)
}

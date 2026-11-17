package store_test

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
	"time"
)

func TestWhatsAppOutboxLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(source.ID, "15557654321@s.whatsapp.net", "direct_chat", "")
	require.NoError(err)

	id, created, err := st.InsertWhatsAppOutboxIfAbsent(ctx, store.WhatsAppOutboxInsert{
		LocalRequestID: "req-1",
		SourceID:       source.ID,
		ConversationID: sql.NullInt64{Int64: convID, Valid: true},
		Kind:           store.WhatsAppOutboxMessage,
		ChatJID:        "15557654321@s.whatsapp.net",
		Body:           sql.NullString{String: "hello", Valid: true},
	})
	require.NoError(err)
	require.NotZero(id)
	assert.True(created)

	dupID, dupCreated, err := st.InsertWhatsAppOutboxIfAbsent(ctx, store.WhatsAppOutboxInsert{
		LocalRequestID: "req-1",
		SourceID:       source.ID,
		Kind:           store.WhatsAppOutboxMessage,
		ChatJID:        "15557654321@s.whatsapp.net",
		Body:           sql.NullString{String: "ignored", Valid: true},
	})
	require.NoError(err)
	assert.Equal(id, dupID)
	assert.False(dupCreated)
	require.NoError(st.MarkWhatsAppOutboxSending(ctx, id))
	rec, err := st.GetWhatsAppOutbox(ctx, id)
	require.NoError(err)
	assert.Equal(store.WhatsAppOutboxSending, rec.Status)
	assert.True(rec.CreatedAt.Before(rec.UpdatedAt) || rec.CreatedAt.Equal(rec.UpdatedAt))

	msgID, err := st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: store.WhatsAppSourceMessageID("15557654321@s.whatsapp.net", "remote-1"),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: time.Unix(10, 0), Valid: true},
	})
	require.NoError(err)
	require.NoError(st.MarkWhatsAppOutboxSent(ctx, id, "remote-1", msgID))
	rec, err = st.GetWhatsAppOutbox(ctx, id)
	require.NoError(err)
	assert.Equal(store.WhatsAppOutboxSent, rec.Status)
	assert.Equal("remote-1", rec.RemoteMessageID.String)
	assert.Equal(msgID, rec.MessageID.Int64)
}

func TestSetReactionReplaceAndClear(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(source.ID, "15557654321@s.whatsapp.net", "direct_chat", "")
	require.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15557654321@s.whatsapp.net", "")
	require.NoError(err)
	msgID, err := st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: store.WhatsAppSourceMessageID("15557654321@s.whatsapp.net", "msg-1"),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: time.Unix(10, 0), Valid: true},
	})
	require.NoError(err)
	require.NoError(st.SetReaction(msgID, participantID, "emoji", "👍", time.Unix(20, 0)))
	require.NoError(st.SetReaction(msgID, participantID, "emoji", "❤️", time.Unix(30, 0)))

	var activeValue string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT reaction_value FROM reactions
		WHERE message_id = ? AND participant_id = ? AND removed_at IS NULL
	`), msgID, participantID).Scan(&activeValue))
	assert.Equal("❤️", activeValue)

	var removedCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM reactions
		WHERE message_id = ? AND participant_id = ? AND reaction_value = ? AND removed_at IS NOT NULL
	`), msgID, participantID, "👍").Scan(&removedCount))
	assert.Equal(1, removedCount)
	require.NoError(st.SetReaction(msgID, participantID, "emoji", "", time.Unix(40, 0)))
	var activeCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM reactions
		WHERE message_id = ? AND participant_id = ? AND removed_at IS NULL
	`), msgID, participantID).Scan(&activeCount))
	assert.Equal(0, activeCount)
}

func TestGetOldestWhatsAppMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	require.NoError(err)
	chatJID := "15557654321@s.whatsapp.net"
	convID, err := st.EnsureConversationWithType(source.ID, chatJID, "direct_chat", "")
	require.NoError(err)

	anchor, err := st.GetOldestWhatsAppMessage(ctx, source.ID, chatJID)
	require.NoError(err)
	assert.Nil(anchor, "no archived messages yet should report no anchor, not an error")

	older := time.Unix(100, 0)
	newer := time.Unix(200, 0)
	_, err = st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: store.WhatsAppSourceMessageID(chatJID, "newer-msg"),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: newer, Valid: true},
		IsFromMe:        false,
	})
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: store.WhatsAppSourceMessageID(chatJID, "older-msg"),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: older, Valid: true},
		IsFromMe:        true,
	})
	require.NoError(err)

	anchor, err = st.GetOldestWhatsAppMessage(ctx, source.ID, chatJID)
	require.NoError(err)
	require.NotNil(anchor)
	assert.Equal(store.WhatsAppSourceMessageID(chatJID, "older-msg"), anchor.SourceMessageID)
	assert.True(anchor.IsFromMe)
	assert.True(older.Equal(anchor.SentAt))

	// A different chat within the same source must not see this chat's anchor.
	otherAnchor, err := st.GetOldestWhatsAppMessage(ctx, source.ID, "15550000000@s.whatsapp.net")
	require.NoError(err)
	assert.Nil(otherAnchor)
}

func TestGetWhatsAppMessageRef(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(source.ID, "120363@g.us", "group_chat", "")
	require.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15557654321@s.whatsapp.net", "")
	require.NoError(err)

	sourceMessageID := store.WhatsAppSourceMessageID("120363@g.us", "ABCDEF")
	msgID, err := st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: sourceMessageID,
		MessageType:     store.WhatsAppMessageType,
		SenderID:        sql.NullInt64{Int64: participantID, Valid: true},
		SentAt:          sql.NullTime{Time: time.Unix(10, 0), Valid: true},
	})
	require.NoError(err)

	gotID, err := st.GetWhatsAppMessageIDBySource(ctx, source.ID, sourceMessageID)
	require.NoError(err)
	assert.Equal(msgID, gotID)

	ref, err := st.GetWhatsAppMessageRef(ctx, msgID)
	require.NoError(err)
	assert.Equal(msgID, ref.ID)
	assert.Equal("120363@g.us", ref.ChatJID)
	assert.Equal("ABCDEF", ref.RemoteMessageID)
	assert.Equal("15557654321@s.whatsapp.net", ref.SenderJID)
}

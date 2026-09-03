package mcp

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

func whatsAppOnlyOptions(t *testing.T) ServeOptions {
	t.Helper()
	f := storetest.New(t)
	return ServeOptions{
		Engine: &querytest.MockEngine{},
		WhatsAppFactory: func(context.Context, string) (whatsapplive.Client, error) {
			return &mockWhatsAppClient{}, nil
		},
		WhatsAppArchive: f.Store,
		ToolAllowlist:   WhatsAppToolNames(),
	}
}

func listedToolNames(t *testing.T, tools []map[string]any) []string {
	t.Helper()
	names := make([]string, 0, len(tools))
	for name := range toolsByName(t, tools) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestToolAllowlistServesOnlyWhatsAppTools(t *testing.T) {
	opts := whatsAppOnlyOptions(t)

	want := WhatsAppToolNames()
	sort.Strings(want)
	assert.Equal(t, want, listedToolNames(t, rawListTools(t, opts, true)))

	assert.Equal(t,
		[]string{ToolListWhatsAppChats, ToolListWhatsAppMessages, ToolWhatsAppLoginStatus, ToolWhatsAppStatus},
		listedToolNames(t, rawListTools(t, opts, false)),
		"write tools stay gated by allowWrites inside the allowlist")

	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	templates, raw := task4RawRequest(t, handler, "resources/templates/list", nil, nil)
	require.Empty(t, templates.Error, "response: %s", raw)
	assert.Empty(t, templates.Result["resourceTemplates"], "allowlisted server registers no attachment resources")
}

func TestEmptyToolAllowlistKeepsFullCatalog(t *testing.T) {
	opts := whatsAppOnlyOptions(t)
	opts.ToolAllowlist = nil

	byName := toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(t, byName, ToolSearchMetadata)
	assert.Contains(t, byName, ToolSendWhatsAppMessage)
	assert.Contains(t, byName, ToolListWhatsAppChats)

	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	templates, raw := task4RawRequest(t, handler, "resources/templates/list", nil, nil)
	require.Empty(t, templates.Error, "response: %s", raw)
	assert.Len(t, templates.Result["resourceTemplates"], 1)
}

func TestWhatsAppArchiveReadToolsScopeToWhatsApp(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	const personal = "15551234567@s.whatsapp.net"
	const work = "15559876543@s.whatsapp.net"
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, personal)
	require.NoError(t, err)
	workSource, err := st.GetOrCreateSource(store.WhatsAppSourceType, work)
	require.NoError(t, err)
	senderID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15557654321@s.whatsapp.net", "Alice")
	require.NoError(t, err)

	quietJID := "15550000001@s.whatsapp.net"
	busyJID := "120363000000000001@g.us"
	quietConv, err := st.EnsureConversationWithType(source.ID, quietJID, "direct_chat", "")
	require.NoError(t, err)
	busyConv, err := st.EnsureConversationWithType(source.ID, busyJID, "group_chat", "Family")
	require.NoError(t, err)
	// The work account is a member of the same group, so the JID is shared.
	workConv, err := st.EnsureConversationWithType(workSource.ID, busyJID, "group_chat", "Family")
	require.NoError(t, err)

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	insert := func(sourceID, convID int64, chatJID, remoteID string, at time.Time, fromMe bool, body string) int64 {
		t.Helper()
		msg := &store.Message{
			SourceID:        sourceID,
			ConversationID:  convID,
			SourceMessageID: store.WhatsAppSourceMessageID(chatJID, remoteID),
			MessageType:     store.WhatsAppMessageType,
			SentAt:          sql.NullTime{Time: at, Valid: true},
			IsFromMe:        fromMe,
			Snippet:         sql.NullString{String: body, Valid: true},
		}
		if !fromMe {
			msg.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
		}
		id, err := st.UpsertMessage(msg)
		require.NoError(t, err)
		require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		return id
	}
	quietID := insert(source.ID, quietConv, quietJID, "Q1", base, true, "hello quiet")
	first := insert(source.ID, busyConv, busyJID, "B1", base.Add(time.Hour), false, "first")
	second := insert(source.ID, busyConv, busyJID, "B2", base.Add(2*time.Hour), true, "second")
	workID := insert(workSource.ID, workConv, busyJID, "W1", base.Add(3*time.Hour), false, "seen from work")
	require.NoError(t, st.RecomputeConversationStats(source.ID))
	require.NoError(t, st.RecomputeConversationStats(workSource.ID))
	// The fixture's Gmail source must never leak into the WhatsApp-scoped tools.
	f.CreateMessage("email-1")

	h := &handlers{engine: &querytest.MockEngine{}, whatsAppArchive: st}

	chats := runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats, map[string]any{})
	require.Len(t, chats.Chats, 3)
	assert.False(t, chats.HasMore)
	assert.Equal(t, 3, chats.NextOffset)
	assert.Equal(t, work, chats.Chats[0].Account, "most recent activity first")
	assert.Equal(t, busyJID, chats.Chats[1].ChatJID)
	assert.Equal(t, personal, chats.Chats[1].Account)
	assert.Equal(t, "Family", chats.Chats[1].Title)
	assert.Equal(t, "group_chat", chats.Chats[1].ConversationType)
	assert.Equal(t, int64(2), chats.Chats[1].MessageCount)
	require.NotNil(t, chats.Chats[1].LastMessageAt)
	assert.True(t, base.Add(2*time.Hour).Equal(*chats.Chats[1].LastMessageAt))
	assert.Equal(t, quietJID, chats.Chats[2].ChatJID)

	scoped := runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"account": personal})
	require.Len(t, scoped.Chats, 2)
	for _, chat := range scoped.Chats {
		assert.Equal(t, personal, chat.Account)
	}

	byTitle := runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"query": "FAM", "account": personal})
	require.Len(t, byTitle.Chats, 1)
	assert.Equal(t, busyJID, byTitle.Chats[0].ChatJID)
	byJID := runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"query": "15550000001"})
	require.Len(t, byJID.Chats, 1)
	assert.Equal(t, quietJID, byJID.Chats[0].ChatJID)
	noMatch := runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"query": "%_nothing"})
	assert.Empty(t, noMatch.Chats, "LIKE metacharacters are matched literally")

	pageOne := runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"limit": float64(2)})
	require.Len(t, pageOne.Chats, 2)
	assert.True(t, pageOne.HasMore)
	assert.Equal(t, 2, pageOne.NextOffset)
	pageTwo := runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"limit": float64(2), "offset": float64(pageOne.NextOffset)})
	require.Len(t, pageTwo.Chats, 1)
	assert.Equal(t, quietJID, pageTwo.Chats[0].ChatJID)
	assert.False(t, pageTwo.HasMore)
	assert.Equal(t, 3, pageTwo.NextOffset)
	pastEnd := runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"offset": float64(pageTwo.NextOffset)})
	assert.Empty(t, pastEnd.Chats)
	assert.False(t, pastEnd.HasMore)
	assert.Equal(t, 3, pastEnd.NextOffset)
	badOffset := runToolExpectError(t, ToolListWhatsAppChats, h.listWhatsAppChats, map[string]any{"offset": float64(-1)})
	assert.Contains(t, resultText(t, badOffset), "offset must be a non-negative integer")

	page := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "limit": float64(1)})
	require.Len(t, page.Messages, 1)
	assert.Equal(t, first, page.Messages[0].ID)
	assert.Equal(t, personal, page.Messages[0].Account)
	assert.Equal(t, busyJID, page.Messages[0].ChatJID)
	assert.Equal(t, store.WhatsAppSourceMessageID(busyJID, "B1"), page.Messages[0].SourceMessageID)
	assert.Equal(t, "first", page.Messages[0].Body)
	assert.Equal(t, "15557654321@s.whatsapp.net", page.Messages[0].SenderJID)
	assert.Equal(t, "Alice", page.Messages[0].SenderName)
	assert.False(t, page.Messages[0].IsFromMe)
	require.NotNil(t, page.Messages[0].SentAt)
	assert.True(t, base.Add(time.Hour).Equal(*page.Messages[0].SentAt))
	assert.Equal(t, first, page.NextAfterID)

	next := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "after_id": float64(page.NextAfterID)})
	require.Len(t, next.Messages, 2, "without account the shared JID mixes both accounts")
	assert.Equal(t, second, next.Messages[0].ID)
	assert.True(t, next.Messages[0].IsFromMe)
	assert.Empty(t, next.Messages[0].SenderJID)
	assert.Equal(t, workID, next.Messages[1].ID)
	assert.Equal(t, work, next.Messages[1].Account)
	assert.Equal(t, workID, next.NextAfterID)

	personalOnly := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "account": personal})
	require.Len(t, personalOnly.Messages, 2)
	assert.Equal(t, first, personalOnly.Messages[0].ID)
	assert.Equal(t, second, personalOnly.Messages[1].ID)
	assert.Equal(t, second, personalOnly.NextAfterID)
	workOnly := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "account": work})
	require.Len(t, workOnly.Messages, 1)
	assert.Equal(t, workID, workOnly.Messages[0].ID)
	assert.Equal(t, "seen from work", workOnly.Messages[0].Body)

	empty := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "after_id": float64(workID)})
	assert.Empty(t, empty.Messages)
	assert.Equal(t, workID, empty.NextAfterID, "empty page echoes the requested cursor")

	quiet := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": quietJID})
	require.Len(t, quiet.Messages, 1)
	assert.Equal(t, quietID, quiet.Messages[0].ID)

	missing := runToolExpectError(t, ToolListWhatsAppMessages, h.listWhatsAppMessages, map[string]any{})
	assert.Contains(t, resultText(t, missing), "chat_jid is required")
	negative := runToolExpectError(t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "after_id": float64(-1)})
	assert.Contains(t, resultText(t, negative), "after_id must be a non-negative integer")
	fractional := runToolExpectError(t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "after_id": 1.5})
	assert.Contains(t, resultText(t, fractional), "after_id must be a non-negative integer")
}

func TestWhatsAppArchiveReadToolsRequireArchive(t *testing.T) {
	h := &handlers{engine: &querytest.MockEngine{}}
	chats := runToolExpectError(t, ToolListWhatsAppChats, h.listWhatsAppChats, map[string]any{})
	assert.Contains(t, resultText(t, chats), "not configured")
	messages := runToolExpectError(t, ToolListWhatsAppMessages, h.listWhatsAppMessages, map[string]any{"chat_jid": "x@g.us"})
	assert.Contains(t, resultText(t, messages), "not configured")
}

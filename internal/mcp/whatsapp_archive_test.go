package mcp

import (
	"context"
	"database/sql"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
	"sort"
	"testing"
	"time"
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
	assert := assert.New(t)
	opts := whatsAppOnlyOptions(t)

	want := WhatsAppToolNames()
	sort.Strings(want)
	assert.Equal(want, listedToolNames(t, rawListTools(t, opts, true)))
	assert.Equal(
		[]string{ToolListWhatsAppChats, ToolListWhatsAppMessages, ToolWhatsAppLoginStatus, ToolWhatsAppStatus},
		listedToolNames(t, rawListTools(t, opts, false)),
		"write tools stay gated by allowWrites inside the allowlist")

	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	templates, raw := task4RawRequest(t, handler, "resources/templates/list", nil, nil)
	require.Empty(t, templates.Error, "response: %s", raw)
	assert.Empty(templates.Result["resourceTemplates"], "allowlisted server registers no attachment resources")
}

func TestEmptyToolAllowlistKeepsFullCatalog(t *testing.T) {
	assert := assert.New(t)
	opts := whatsAppOnlyOptions(t)
	opts.ToolAllowlist = nil

	byName := toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(byName, ToolSearchMetadata)
	assert.Contains(byName, ToolSendWhatsAppMessage)
	assert.Contains(byName, ToolListWhatsAppChats)

	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	templates, raw := task4RawRequest(t, handler, "resources/templates/list", nil, nil)
	require.Empty(t, templates.Error, "response: %s", raw)
	assert.Len(templates.Result["resourceTemplates"], 1)
}

func TestWhatsAppArchiveReadToolsScopeToWhatsApp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	const personal = "15551234567@s.whatsapp.net"
	const work = "15559876543@s.whatsapp.net"
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, personal)
	require.NoError(err)
	workSource, err := st.GetOrCreateSource(store.WhatsAppSourceType, work)
	require.NoError(err)
	senderID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15557654321@s.whatsapp.net", "Alice")
	require.NoError(err)

	quietJID := "15550000001@s.whatsapp.net"
	busyJID := "120363000000000001@g.us"
	quietConv, err := st.EnsureConversationWithType(source.ID, quietJID, "direct_chat", "")
	require.NoError(err)
	busyConv, err := st.EnsureConversationWithType(source.ID, busyJID, "group_chat", "Family")
	require.NoError(err)
	// The work account is a member of the same group, so the JID is shared.
	workConv, err := st.EnsureConversationWithType(workSource.ID, busyJID, "group_chat", "Family")
	require.NoError(err)

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
		require.NoError(err)
		require.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		return id
	}
	quietID := insert(source.ID, quietConv, quietJID, "Q1", base, true, "hello quiet")
	first := insert(source.ID, busyConv, busyJID, "B1", base.Add(time.Hour), false, "first")
	second := insert(source.ID, busyConv, busyJID, "B2", base.Add(2*time.Hour), true, "second")
	workID := insert(workSource.ID, workConv, busyJID, "W1", base.Add(3*time.Hour), false, "seen from work")
	require.NoError(st.RecomputeConversationStats(source.ID))
	require.NoError(st.RecomputeConversationStats(workSource.ID))
	// The fixture's Gmail source must never leak into the WhatsApp-scoped tools.
	f.CreateMessage("email-1")

	// Chats with no archived messages have no activity and must sort last,
	// newest conversation first. The accented title exercises Unicode-aware
	// matching, which SQLite's built-in LOWER does not provide.
	idleJID := "15550000002@s.whatsapp.net"
	_, err = st.EnsureConversationWithType(source.ID, idleJID, "direct_chat", "École Parents")
	require.NoError(err)
	idle2JID := "15550000003@s.whatsapp.net"
	_, err = st.EnsureConversationWithType(source.ID, idle2JID, "direct_chat", "Sans activisté")
	require.NoError(err)

	h := &handlers{engine: &querytest.MockEngine{}, whatsAppArchive: st}
	listChats := func(args map[string]any) listWhatsAppChatsResponse {
		t.Helper()
		return runTool[listWhatsAppChatsResponse](t, ToolListWhatsAppChats, h.listWhatsAppChats, args)
	}
	chatJIDs := func(page listWhatsAppChatsResponse) []string {
		jids := make([]string, 0, len(page.Chats))
		for _, chat := range page.Chats {
			jids = append(jids, chat.Account+"/"+chat.ChatJID)
		}
		return jids
	}
	fullOrder := []string{
		work + "/" + busyJID, personal + "/" + busyJID, personal + "/" + quietJID,
		personal + "/" + idle2JID, personal + "/" + idleJID,
	}

	chats := listChats(map[string]any{})
	assert.Equal(fullOrder, chatJIDs(chats), "most recent activity first, no activity last")
	assert.False(chats.HasMore)
	assert.Empty(chats.NextCursor)
	assert.Equal("Family", chats.Chats[1].Title)
	assert.Equal("group_chat", chats.Chats[1].ConversationType)
	assert.Equal(int64(2), chats.Chats[1].MessageCount)
	require.NotNil(chats.Chats[1].LastMessageAt)
	assert.True(base.Add(2 * time.Hour).Equal(*chats.Chats[1].LastMessageAt))
	assert.Nil(chats.Chats[4].LastMessageAt)

	scoped := listChats(map[string]any{"account": personal})
	assert.Equal(fullOrder[1:], chatJIDs(scoped))

	byUnicodeTitle := listChats(map[string]any{"query": "ÉCOLE"})
	assert.Equal([]string{personal + "/" + idleJID}, chatJIDs(byUnicodeTitle), "Unicode case folding on title")
	byTitle := listChats(map[string]any{"query": "fam", "account": personal})
	assert.Equal([]string{personal + "/" + busyJID}, chatJIDs(byTitle))
	byJID := listChats(map[string]any{"query": "15550000001"})
	assert.Equal([]string{personal + "/" + quietJID}, chatJIDs(byJID))
	assert.Empty(listChats(map[string]any{"query": "%_nothing"}).Chats, "LIKE metacharacters are matched literally")

	// Walk the whole list one chat per page, through the no-activity tail.
	var walked []string
	walkArgs := map[string]any{"limit": float64(1)}
	for range 10 {
		page := listChats(walkArgs)
		walked = append(walked, chatJIDs(page)...)
		if !page.HasMore {
			assert.Empty(page.NextCursor)
			break
		}
		require.NotEmpty(page.NextCursor)
		walkArgs = map[string]any{"limit": float64(1), "cursor": page.NextCursor}
	}
	assert.Equal(fullOrder, walked, "keyset paging visits every chat exactly once")

	pageOne := listChats(map[string]any{"limit": float64(2)})
	assert.Equal(fullOrder[:2], chatJIDs(pageOne))
	require.True(pageOne.HasMore)
	pageTwo := listChats(map[string]any{"limit": float64(2), "cursor": pageOne.NextCursor})
	assert.Equal(fullOrder[2:4], chatJIDs(pageTwo))
	assert.True(pageTwo.HasMore)

	foreignCursor := runToolExpectError(t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"cursor": pageOne.NextCursor, "query": "fam"})
	assert.Contains(resultText(t, foreignCursor), "invalid_cursor")
	garbageCursor := runToolExpectError(t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"cursor": "not-a-cursor"})
	assert.Contains(resultText(t, garbageCursor), "invalid_cursor")

	// order=created walks by immutable conversation id, oldest first.
	createdOrder := []string{
		personal + "/" + quietJID, personal + "/" + busyJID, work + "/" + busyJID,
		personal + "/" + idleJID, personal + "/" + idle2JID,
	}
	assert.Equal(createdOrder, chatJIDs(listChats(map[string]any{"order": "created"})))
	createdPageOne := listChats(map[string]any{"order": "created", "limit": float64(2)})
	assert.Equal(createdOrder[:2], chatJIDs(createdPageOne))
	require.True(createdPageOne.HasMore)
	crossOrder := runToolExpectError(t, ToolListWhatsAppChats, h.listWhatsAppChats,
		map[string]any{"cursor": createdPageOne.NextCursor})
	assert.Contains(resultText(t, crossOrder), "invalid_cursor", "a created-order cursor cannot resume a recent-order walk")
	badOrder := runToolExpectError(t, ToolListWhatsAppChats, h.listWhatsAppChats, map[string]any{"order": "random"})
	assert.Contains(resultText(t, badOrder), "invalid_order")

	// New activity reorders the recent listing between pages: the quiet chat
	// jumps to the top. A recent-order page-one cursor neither repeats nor
	// skips the chats that did not move, but the moved chat is not revisited
	// by that walk (documented best-effort); it leads a fresh listing.
	lateID := insert(source.ID, quietConv, quietJID, "Q2", base.Add(5*time.Hour), false, "late")
	require.NoError(st.RecomputeConversationStats(source.ID))
	pageTwoAfterMove := listChats(map[string]any{"limit": float64(2), "cursor": pageOne.NextCursor})
	assert.Equal(fullOrder[3:], chatJIDs(pageTwoAfterMove))
	assert.False(pageTwoAfterMove.HasMore)
	assert.Equal([]string{personal + "/" + quietJID, fullOrder[0], fullOrder[1]},
		chatJIDs(listChats(map[string]any{"limit": float64(3)})))

	// The same mutation, plus a chat created mid-walk, leaves a created-order
	// walk exhaustive: the moved chat was already visited, the idle chats
	// still follow, and the new chat appears at the end.
	newJID := "15550000004@s.whatsapp.net"
	_, err = st.EnsureConversationWithType(source.ID, newJID, "direct_chat", "Brand new")
	require.NoError(err)
	createdPageTwo := listChats(map[string]any{"order": "created", "limit": float64(2), "cursor": createdPageOne.NextCursor})
	assert.Equal(createdOrder[2:4], chatJIDs(createdPageTwo))
	require.True(createdPageTwo.HasMore)
	createdPageThree := listChats(map[string]any{"order": "created", "limit": float64(2), "cursor": createdPageTwo.NextCursor})
	assert.Equal([]string{createdOrder[4], personal + "/" + newJID}, chatJIDs(createdPageThree))
	assert.False(createdPageThree.HasMore)

	page := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "limit": float64(1)})
	require.Len(page.Messages, 1)
	assert.Equal(first, page.Messages[0].ID)
	assert.Equal(personal, page.Messages[0].Account)
	assert.Equal(busyJID, page.Messages[0].ChatJID)
	assert.Equal(store.WhatsAppSourceMessageID(busyJID, "B1"), page.Messages[0].SourceMessageID)
	assert.Equal("first", page.Messages[0].Body)
	assert.Equal("15557654321@s.whatsapp.net", page.Messages[0].SenderJID)
	assert.Equal("Alice", page.Messages[0].SenderName)
	assert.False(page.Messages[0].IsFromMe)
	require.NotNil(page.Messages[0].SentAt)
	assert.True(base.Add(time.Hour).Equal(*page.Messages[0].SentAt))
	assert.Equal(first, page.NextAfterID)

	next := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "after_id": float64(page.NextAfterID)})
	require.Len(next.Messages, 2, "without account the shared JID mixes both accounts")
	assert.Equal(second, next.Messages[0].ID)
	assert.True(next.Messages[0].IsFromMe)
	assert.Empty(next.Messages[0].SenderJID)
	assert.Equal(workID, next.Messages[1].ID)
	assert.Equal(work, next.Messages[1].Account)
	assert.Equal(workID, next.NextAfterID)

	personalOnly := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "account": personal})
	require.Len(personalOnly.Messages, 2)
	assert.Equal(first, personalOnly.Messages[0].ID)
	assert.Equal(second, personalOnly.Messages[1].ID)
	assert.Equal(second, personalOnly.NextAfterID)
	workOnly := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "account": work})
	require.Len(workOnly.Messages, 1)
	assert.Equal(workID, workOnly.Messages[0].ID)
	assert.Equal("seen from work", workOnly.Messages[0].Body)

	empty := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "after_id": float64(workID)})
	assert.Empty(empty.Messages)
	assert.Equal(workID, empty.NextAfterID, "empty page echoes the requested cursor")

	quiet := runTool[listWhatsAppMessagesResponse](t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": quietJID})
	require.Len(quiet.Messages, 2)
	assert.Equal(quietID, quiet.Messages[0].ID)
	assert.Equal(lateID, quiet.Messages[1].ID)
	assert.Equal(lateID, quiet.NextAfterID)

	missing := runToolExpectError(t, ToolListWhatsAppMessages, h.listWhatsAppMessages, map[string]any{})
	assert.Contains(resultText(t, missing), "chat_jid is required")
	negative := runToolExpectError(t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "after_id": float64(-1)})
	assert.Contains(resultText(t, negative), "after_id must be a non-negative integer")
	fractional := runToolExpectError(t, ToolListWhatsAppMessages, h.listWhatsAppMessages,
		map[string]any{"chat_jid": busyJID, "after_id": 1.5})
	assert.Contains(resultText(t, fractional), "after_id must be a non-negative integer")
}

func TestWhatsAppArchiveReadToolsRequireArchive(t *testing.T) {
	h := &handlers{engine: &querytest.MockEngine{}}
	chats := runToolExpectError(t, ToolListWhatsAppChats, h.listWhatsAppChats, map[string]any{})
	assert.Contains(t, resultText(t, chats), "not configured")
	messages := runToolExpectError(t, ToolListWhatsAppMessages, h.listWhatsAppMessages, map[string]any{"chat_jid": "x@g.us"})
	assert.Contains(t, resultText(t, messages), "not configured")
}

// Owner integrations must remain absent even if a caller supplies their factories
// alongside a delegated grant. Managed drafts use their own scoped runner.
func TestDelegatedCatalogExcludesForkIntegrations(t *testing.T) {
	opts := whatsAppOnlyOptions(t)
	opts.ToolAllowlist = nil
	opts.DelegatedOnly = true
	opts.GmailFactory = func(context.Context, string) (gmail.LiveAPI, error) {
		return nil, errors.New("delegated sessions must not create live mail clients")
	}
	assert.Empty(t, rawListTools(t, opts, true))
}

package live

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type mockTransport struct {
	status             Status
	connect            func(context.Context) error
	logout             func(context.Context, TransportLogoutRequest) (TransportLogoutResult, error)
	startQRPairing     func(context.Context) error
	pairingState       QRPairingState
	sendMessage        func(context.Context, TransportSendMessageRequest) (TransportSendResult, error)
	sendReaction       func(context.Context, TransportSendReactionRequest) (TransportSendResult, error)
	createGroup        func(context.Context, TransportCreateGroupRequest) (TransportCreateGroupResult, error)
	requestHistorySync func(context.Context, TransportRequestHistorySyncRequest) error
}

func (m *mockTransport) Status(context.Context) (Status, error) { return m.status, nil }
func (m *mockTransport) Connect(ctx context.Context) error {
	if m.connect != nil {
		return m.connect(ctx)
	}
	return nil
}
func (m *mockTransport) Close() error { return nil }
func (m *mockTransport) Logout(ctx context.Context, req TransportLogoutRequest) (TransportLogoutResult, error) {
	if m.logout != nil {
		return m.logout(ctx, req)
	}
	return TransportLogoutResult{}, nil
}
func (m *mockTransport) StartQRPairing(ctx context.Context) error {
	if m.startQRPairing != nil {
		return m.startQRPairing(ctx)
	}
	return nil
}
func (m *mockTransport) PairingState(context.Context) (QRPairingState, error) {
	return m.pairingState, nil
}
func (m *mockTransport) SendMessage(ctx context.Context, req TransportSendMessageRequest) (TransportSendResult, error) {
	return m.sendMessage(ctx, req)
}
func (m *mockTransport) SendReaction(ctx context.Context, req TransportSendReactionRequest) (TransportSendResult, error) {
	return m.sendReaction(ctx, req)
}
func (m *mockTransport) CreateGroup(ctx context.Context, req TransportCreateGroupRequest) (TransportCreateGroupResult, error) {
	if m.createGroup != nil {
		return m.createGroup(ctx, req)
	}
	return TransportCreateGroupResult{}, nil
}

// RequestHistorySync makes mockTransport satisfy the optional
// HistorySyncTransport interface. Tests that don't care about this path
// leave requestHistorySync nil and get a no-op success; tests exercising
// Service.RequestHistorySync set it to capture/assert the built request. See
// minimalTransport below for a transport that deliberately does NOT
// implement HistorySyncTransport.
func (m *mockTransport) RequestHistorySync(ctx context.Context, req TransportRequestHistorySyncRequest) error {
	if m.requestHistorySync != nil {
		return m.requestHistorySync(ctx, req)
	}
	return nil
}

// minimalTransport implements only the core Transport interface (no
// HistorySyncTransport), used to verify Service.RequestHistorySync's error
// when the configured transport doesn't support history-sync requests.
type minimalTransport struct {
	status Status
}

func (m *minimalTransport) Status(context.Context) (Status, error) { return m.status, nil }
func (m *minimalTransport) Connect(context.Context) error          { return nil }
func (m *minimalTransport) Close() error                           { return nil }
func (m *minimalTransport) Logout(context.Context, TransportLogoutRequest) (TransportLogoutResult, error) {
	return TransportLogoutResult{}, nil
}
func (m *minimalTransport) SendMessage(context.Context, TransportSendMessageRequest) (TransportSendResult, error) {
	return TransportSendResult{}, nil
}
func (m *minimalTransport) SendReaction(context.Context, TransportSendReactionRequest) (TransportSendResult, error) {
	return TransportSendResult{}, nil
}
func (m *minimalTransport) CreateGroup(context.Context, TransportCreateGroupRequest) (TransportCreateGroupResult, error) {
	return TransportCreateGroupResult{}, nil
}

func TestServiceSendMessageArchivesAndOutbox(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	sentAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	sendCalls := 0
	transport := &mockTransport{
		status: Status{
			AccountJID: "15551234567@s.whatsapp.net",
			Paired:     true,
			Connected:  true,
			LoggedIn:   true,
		},
		sendMessage: func(_ context.Context, req TransportSendMessageRequest) (TransportSendResult, error) {
			sendCalls++
			assert.Equal("15557654321@s.whatsapp.net", req.ChatID)
			assert.Equal("hello", req.Body)
			return TransportSendResult{
				RemoteMessageID: "remote-1",
				ChatJID:         req.ChatID,
				Timestamp:       sentAt,
			}, nil
		},
	}
	svc, err := NewService(ServiceOptions{
		Store:     st,
		Transport: transport,
		Now:       func() time.Time { return sentAt },
	})
	require.NoError(err)

	result, err := svc.SendMessage(ctx, SendMessageRequest{
		ChatID:         "15557654321@s.whatsapp.net",
		Body:           "hello",
		LocalRequestID: "req-1",
	})
	require.NoError(err)
	assert.Equal(store.WhatsAppOutboxSent, result.Status)
	assert.Equal("remote-1", result.RemoteMessageID)
	assert.NotZero(result.MessageID)

	duplicate, err := svc.SendMessage(ctx, SendMessageRequest{
		ChatID:         "15557654321@s.whatsapp.net",
		Body:           "hello",
		LocalRequestID: "req-1",
	})
	require.NoError(err)
	assert.Equal(result, duplicate)
	assert.Equal(1, sendCalls)

	outbox, err := st.GetWhatsAppOutbox(ctx, result.OutboxID)
	require.NoError(err)
	assert.Equal(store.WhatsAppOutboxSent, outbox.Status)
	assert.Equal(result.MessageID, outbox.MessageID.Int64)

	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text
		FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.id = ?
	`), result.MessageID).Scan(&body))
	assert.Equal("hello", body)

	ref, err := st.GetWhatsAppMessageRef(ctx, result.MessageID)
	require.NoError(err)
	assert.Equal("15557654321@s.whatsapp.net", ref.ChatJID)
	assert.Equal("remote-1", ref.RemoteMessageID)
	assert.True(ref.IsFromMe)
}

func TestServiceSendMessageRejectsNotReady(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	var called bool
	transport := &mockTransport{
		status: Status{
			AccountJID: "15551234567@s.whatsapp.net",
			Paired:     true,
			Connected:  true,
			LoggedIn:   false,
		},
		sendMessage: func(context.Context, TransportSendMessageRequest) (TransportSendResult, error) {
			called = true
			return TransportSendResult{}, nil
		},
	}
	svc, err := NewService(ServiceOptions{
		Store:     st,
		Transport: transport,
	})
	require.NoError(err)

	_, err = svc.SendMessage(ctx, SendMessageRequest{
		ChatID: "15557654321@s.whatsapp.net",
		Body:   "hello",
	})
	require.Error(err)
	assert.Contains(err.Error(), "ready=true")
	assert.False(called)
}

// newHistorySyncTestService sets up a store with one archived WhatsApp
// message (usable as a RequestHistorySync anchor) for chatJID, and a Service
// wired to a ready mockTransport. Individual tests override transport.status
// or transport.requestHistorySync as needed.
func newHistorySyncTestService(t *testing.T) (svc *Service, chatJID string, transport *mockTransport) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	require.NoError(t, err)
	chatJID = "15557654321@s.whatsapp.net"
	convID, err := st.EnsureConversationWithType(source.ID, chatJID, "direct_chat", "")
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  convID,
		SourceMessageID: store.WhatsAppSourceMessageID(chatJID, "anchor-msg"),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
		IsFromMe:        true,
	})
	require.NoError(t, err)

	transport = &mockTransport{
		status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true, Connected: true, LoggedIn: true},
	}
	svc, err = NewService(ServiceOptions{Store: st, Transport: transport})
	require.NoError(t, err)
	return svc, chatJID, transport
}

// TestServiceRequestHistorySyncUsesOldestArchivedMessageAsAnchor asserts the
// anchor is the OLDEST archived message for the chat (not, say, the most
// recently inserted one), since WhatsApp's on-demand history-sync returns
// messages strictly before the given anchor.
func TestServiceRequestHistorySyncUsesOldestArchivedMessageAsAnchor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(store.WhatsAppSourceType, "15551234567@s.whatsapp.net")
	require.NoError(err)
	chatJID := "15557654321@s.whatsapp.net"
	convID, err := st.EnsureConversationWithType(source.ID, chatJID, "direct_chat", "")
	require.NoError(err)

	older := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
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

	var captured TransportRequestHistorySyncRequest
	transport := &mockTransport{
		status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true, Connected: true, LoggedIn: true},
		requestHistorySync: func(_ context.Context, req TransportRequestHistorySyncRequest) error {
			captured = req
			return nil
		},
	}
	svc, err := NewService(ServiceOptions{Store: st, Transport: transport})
	require.NoError(err)

	result, err := svc.RequestHistorySync(ctx, RequestHistorySyncRequest{ChatID: chatJID})
	require.NoError(err)
	assert.Equal(chatJID, captured.ChatJID)
	assert.Equal("older-msg", captured.AnchorMessageID)
	assert.True(captured.AnchorIsFromMe)
	assert.True(older.Equal(captured.AnchorTimestamp))
	assert.Equal(DefaultHistorySyncRequestCount, captured.Count)
	assert.Equal(chatJID, result.ChatJID)
	assert.Equal("older-msg", result.AnchorMessageID)
	assert.Equal(DefaultHistorySyncRequestCount, result.RequestedCount)
}

func TestServiceRequestHistorySyncClampsAndDefaultsCount(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero defaults", 0, DefaultHistorySyncRequestCount},
		{"negative defaults", -5, DefaultHistorySyncRequestCount},
		{"within range kept", 30, 30},
		{"over max clamped", 500, MaxHistorySyncRequestCount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, chatJID, _ := newHistorySyncTestService(t)
			result, err := svc.RequestHistorySync(context.Background(), RequestHistorySyncRequest{
				ChatID: chatJID,
				Count:  tt.in,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.want, result.RequestedCount)
		})
	}
}

func TestServiceRequestHistorySyncRejectsNotReady(t *testing.T) {
	svc, chatJID, transport := newHistorySyncTestService(t)
	transport.status.LoggedIn = false
	var called bool
	transport.requestHistorySync = func(context.Context, TransportRequestHistorySyncRequest) error {
		called = true
		return nil
	}

	_, err := svc.RequestHistorySync(context.Background(), RequestHistorySyncRequest{ChatID: chatJID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ready=true")
	assert.False(t, called)
}

func TestServiceRequestHistorySyncRequiresArchivedAnchor(t *testing.T) {
	svc, _, _ := newHistorySyncTestService(t)

	_, err := svc.RequestHistorySync(context.Background(), RequestHistorySyncRequest{ChatID: "15559999999@s.whatsapp.net"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no archived whatsapp messages found")
}

func TestServiceRequestHistorySyncRequiresTransportSupport(t *testing.T) {
	st := testutil.NewTestStore(t)
	transport := &minimalTransport{
		status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true, Connected: true, LoggedIn: true},
	}
	svc, err := NewService(ServiceOptions{Store: st, Transport: transport})
	require.NoError(t, err)

	_, err = svc.RequestHistorySync(context.Background(), RequestHistorySyncRequest{ChatID: "15557654321@s.whatsapp.net"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported by this transport")
}

func TestServiceRequestHistorySyncPropagatesTransportError(t *testing.T) {
	svc, chatJID, transport := newHistorySyncTestService(t)
	transport.requestHistorySync = func(context.Context, TransportRequestHistorySyncRequest) error {
		return assert.AnError
	}

	_, err := svc.RequestHistorySync(context.Background(), RequestHistorySyncRequest{ChatID: chatJID})
	require.Error(t, err)
}

func TestServiceArchiveHistorySyncBatchesStatsAndSkipsNotifications(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	notifications := 0
	transport := &mockTransport{
		status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true},
	}
	svc, err := NewService(ServiceOptions{
		Store:     st,
		Transport: transport,
		Notify: func(context.Context, InboundEvent) {
			notifications++
		},
	})
	require.NoError(err)

	messages := []InboundMessage{
		{
			Account:   "15551234567@s.whatsapp.net",
			ChatJID:   "120363@g.us",
			ChatTitle: "Beynac Team",
			SenderJID: "15557654321@s.whatsapp.net",
			MessageID: "history-1",
			Text:      "first",
			Timestamp: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
			IsGroup:   true,
		},
		{
			Account:   "15551234567@s.whatsapp.net",
			ChatJID:   "120363@g.us",
			ChatTitle: "Beynac Team",
			SenderJID: "15558765432@s.whatsapp.net",
			MessageID: "history-2",
			Text:      "second",
			Timestamp: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
			IsGroup:   true,
		},
	}
	require.NoError(svc.ArchiveHistorySync(ctx, messages))
	require.NoError(svc.ArchiveHistorySync(ctx, messages), "history sync retry is idempotent")
	assert.Zero(notifications)

	var title string
	var messageCount int64
	var preview string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT title, message_count, last_message_preview
		FROM conversations
		WHERE source_conversation_id = ?
	`), "120363@g.us").Scan(&title, &messageCount, &preview))
	assert.Equal("Beynac Team", title)
	assert.Equal(int64(2), messageCount)
	assert.Equal("second", preview)
}

func TestServiceStartLoginKeepsReconnectContextAlive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	loginCtx := t.Context()

	var connectCtx context.Context
	var transport *mockTransport
	transport = &mockTransport{
		status: Status{
			AccountJID: "15551234567@s.whatsapp.net",
			Paired:     true,
			Connected:  false,
			LoggedIn:   true,
		},
		connect: func(ctx context.Context) error {
			connectCtx = ctx
			transport.status.Connected = true
			transport.status.LoggedIn = true
			return nil
		},
	}
	svc, err := NewService(ServiceOptions{
		Store:        st,
		Transport:    transport,
		LoginContext: loginCtx,
	})
	require.NoError(err)

	state, err := svc.StartLogin(ctx)
	require.NoError(err)
	require.NotNil(connectCtx)
	assert.True(state.Status.Ready)
	assert.NoError(connectCtx.Err())
}

func TestServiceSendReactionUpdatesLocalReaction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	var reactionReq TransportSendReactionRequest
	transport := &mockTransport{
		status: Status{
			AccountJID: "15551234567@s.whatsapp.net",
			Paired:     true,
			Connected:  true,
			LoggedIn:   true,
		},
		sendReaction: func(_ context.Context, req TransportSendReactionRequest) (TransportSendResult, error) {
			reactionReq = req
			return TransportSendResult{
				RemoteMessageID: "reaction-1",
				ChatJID:         req.ChatJID,
				Timestamp:       now,
			}, nil
		},
	}
	svc, err := NewService(ServiceOptions{
		Store:     st,
		Transport: transport,
		Now:       func() time.Time { return now },
	})
	require.NoError(err)

	targetID, err := svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "15557654321@s.whatsapp.net",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "target-1",
		Text:      "target",
		Timestamp: now.Add(-time.Minute),
	})
	require.NoError(err)

	result, err := svc.SendReaction(ctx, SendReactionRequest{
		MessageID:      targetID,
		Emoji:          "👍",
		LocalRequestID: "react-1",
	})
	require.NoError(err)
	assert.Equal(store.WhatsAppOutboxSent, result.Status)
	assert.Equal("15557654321@s.whatsapp.net", reactionReq.ChatJID)
	assert.Equal("15557654321@s.whatsapp.net", reactionReq.SenderJID)
	assert.Equal("target-1", reactionReq.RemoteMessageID)
	assert.Equal("👍", reactionReq.Emoji)
	outbox, err := st.GetWhatsAppOutbox(ctx, result.OutboxID)
	require.NoError(err)
	assert.Equal(targetID, outbox.MessageID.Int64)

	selfID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15551234567@s.whatsapp.net", "")
	require.NoError(err)
	var active string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT reaction_value
		FROM reactions
		WHERE message_id = ? AND participant_id = ? AND removed_at IS NULL
	`), targetID, selfID).Scan(&active))
	assert.Equal("👍", active)
}

func TestServiceArchiveInboundReactionClearsReaction(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	transport := &mockTransport{
		status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true},
	}
	svc, err := NewService(ServiceOptions{
		Store:     st,
		Transport: transport,
		Now:       func() time.Time { return now },
	})
	require.NoError(err)

	targetID, err := svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "120363@g.us",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "target-1",
		Text:      "target",
		Timestamp: now.Add(-time.Minute),
		IsGroup:   true,
	})
	require.NoError(err)

	_, err = svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "120363@g.us",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "reaction-1",
		Timestamp: now,
		Reaction: &InboundReaction{
			TargetChatJID:   "120363@g.us",
			TargetMessageID: "target-1",
			Emoji:           "❤️",
		},
	})
	require.NoError(err)

	_, err = svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "120363@g.us",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "reaction-2",
		Timestamp: now.Add(time.Second),
		Reaction: &InboundReaction{
			TargetChatJID:   "120363@g.us",
			TargetMessageID: "target-1",
			Emoji:           "",
		},
	})
	require.NoError(err)

	participantID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15557654321@s.whatsapp.net", "")
	require.NoError(err)
	var activeCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM reactions
		WHERE message_id = ? AND participant_id = ? AND removed_at IS NULL
	`), targetID, participantID).Scan(&activeCount))
	assert.Equal(t, 0, activeCount)
}

func TestServiceLogoutReturnsBeforeAndAfterStatus(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	var gotReq TransportLogoutRequest
	transport := &mockTransport{
		status: Status{
			AccountJID: "15551234567@s.whatsapp.net",
			Connected:  true,
			LoggedIn:   true,
			Paired:     true,
		},
	}
	transport.logout = func(_ context.Context, req TransportLogoutRequest) (TransportLogoutResult, error) {
		gotReq = req
		transport.status = Status{}
		return TransportLogoutResult{
			RemoteLogout:        true,
			LocalSessionCleared: true,
		}, nil
	}
	svc, err := NewService(ServiceOptions{
		Store:     st,
		Transport: transport,
	})
	require.NoError(err)

	result, err := svc.Logout(ctx, LogoutRequest{ForceLocal: true})
	require.NoError(err)
	assert.True(gotReq.ForceLocal)
	assert.True(result.StatusBefore.Paired)
	assert.False(result.StatusAfter.Paired)
	assert.True(result.RemoteLogout)
	assert.True(result.LocalSessionCleared)
}

type serviceContextKey string

func TestServiceStartLoginUsesLoginContextForQRPairing(t *testing.T) {
	ctx := context.WithValue(context.Background(), serviceContextKey("scope"), "request")
	loginCtx := context.WithValue(context.Background(), serviceContextKey("scope"), "daemon")
	st := testutil.NewTestStore(t)
	var gotScope any
	transport := &mockTransport{
		status: Status{},
		startQRPairing: func(ctx context.Context) error {
			gotScope = ctx.Value(serviceContextKey("scope"))
			return nil
		},
		pairingState: QRPairingState{Active: true},
	}
	svc, err := NewService(ServiceOptions{
		Store:        st,
		Transport:    transport,
		LoginContext: loginCtx,
	})
	require.NoError(t, err)

	_, err = svc.StartLogin(ctx)
	require.NoError(t, err)
	assert.Equal(t, "daemon", gotScope)
}

func TestServiceStartLoginUsesLoginContextForReconnect(t *testing.T) {
	ctx := context.WithValue(context.Background(), serviceContextKey("scope"), "request")
	loginCtx := context.WithValue(context.Background(), serviceContextKey("scope"), "daemon")
	st := testutil.NewTestStore(t)
	var gotScope any
	transport := &mockTransport{
		status: Status{
			AccountJID: "15551234567@s.whatsapp.net",
			Paired:     true,
			Connected:  false,
		},
		pairingState: QRPairingState{Paired: true},
	}
	transport.connect = func(ctx context.Context) error {
		gotScope = ctx.Value(serviceContextKey("scope"))
		transport.status.Connected = true
		transport.status.LoggedIn = true
		return nil
	}
	svc, err := NewService(ServiceOptions{
		Store:        st,
		Transport:    transport,
		LoginContext: loginCtx,
	})
	require.NoError(t, err)

	_, err = svc.StartLogin(ctx)
	require.NoError(t, err)
	assert.Equal(t, "daemon", gotScope)
}

// attachmentRow is a minimal snapshot of one `attachments` row, read via raw
// SQL because the AttachmentRef/Replace* path this package uses (mirroring
// Discord/Slack/Beeper) has no dedicated single-row typed accessor.
type attachmentRow struct {
	filename    string
	mimeType    string
	storagePath string
	contentHash string
	mediaType   string
	size        int64
}

func queryWhatsAppAttachment(t *testing.T, st *store.Store, messageID int64) (attachmentRow, bool) {
	t.Helper()
	var row attachmentRow
	err := st.DB().QueryRow(st.Rebind(`
		SELECT filename, mime_type, storage_path, COALESCE(content_hash, ''), COALESCE(media_type, ''), size
		FROM attachments
		WHERE message_id = ?
	`), messageID).Scan(&row.filename, &row.mimeType, &row.storagePath, &row.contentHash, &row.mediaType, &row.size)
	if err != nil {
		return attachmentRow{}, false
	}
	return row, true
}

func queryMessageAttachmentStats(t *testing.T, st *store.Store, messageID int64) (hasAttachments bool, count int64) {
	t.Helper()
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT has_attachments, attachment_count FROM messages WHERE id = ?
	`), messageID).Scan(&hasAttachments, &count))
	return hasAttachments, count
}

func TestServiceArchiveInboundStoresDownloadedAttachment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	attachmentsDir := t.TempDir()
	transport := &mockTransport{
		status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true},
	}
	svc, err := NewService(ServiceOptions{
		Store:          st,
		Transport:      transport,
		AttachmentsDir: attachmentsDir,
	})
	require.NoError(err)

	content := []byte("%PDF-1.4 synthetic bank details")
	messageID, err := svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "15557654321@s.whatsapp.net",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "media-1",
		Timestamp: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
		Attachment: &InboundAttachment{
			Filename:  "RIB.pdf",
			MimeType:  "application/pdf",
			MediaType: "document",
			Data:      content,
		},
	})
	require.NoError(err, "a captionless attachment message must still archive")
	require.NotZero(messageID)

	row, ok := queryWhatsAppAttachment(t, st, messageID)
	require.True(ok, "attachment row must exist for downloaded media")
	assert.Equal("RIB.pdf", row.filename)
	assert.Equal("application/pdf", row.mimeType)
	assert.Equal("document", row.mediaType)
	assert.Equal(int64(len(content)), row.size)
	assert.NotEmpty(row.contentHash)

	storedPath, err := export.StoragePath(attachmentsDir, row.contentHash)
	require.NoError(err)
	stored, err := os.ReadFile(storedPath)
	require.NoError(err)
	assert.Equal(content, stored)

	hasAttachments, count := queryMessageAttachmentStats(t, st, messageID)
	assert.True(hasAttachments)
	assert.Equal(int64(1), count)

	var sizeEstimate int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT size_estimate FROM messages WHERE id = ?
	`), messageID).Scan(&sizeEstimate))
	assert.Equal(int64(len(content)), sizeEstimate, "size_estimate must include downloaded attachment bytes")
}

func TestServiceArchiveInboundDownloadFailureLeavesPendingMarkerRow(t *testing.T) {
	tests := []struct {
		name              string
		configureAttchDir bool
		attachment        *InboundAttachment
	}{
		{
			name:              "media key expired",
			configureAttchDir: true,
			attachment: &InboundAttachment{
				Filename:      "RIB.pdf",
				MimeType:      "application/pdf",
				MediaType:     "document",
				DownloadError: "media key expired",
			},
		},
		{
			name:              "attachments dir not configured",
			configureAttchDir: false,
			attachment: &InboundAttachment{
				Filename:  "RIB.pdf",
				MimeType:  "application/pdf",
				MediaType: "document",
				Data:      []byte("downloaded but nowhere to put it"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := context.Background()
			st := testutil.NewTestStore(t)
			var dir string
			if tt.configureAttchDir {
				dir = t.TempDir()
			}
			var gotEvent InboundEvent
			transport := &mockTransport{
				status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true},
			}
			svc, err := NewService(ServiceOptions{
				Store:          st,
				Transport:      transport,
				AttachmentsDir: dir,
				Notify:         func(_ context.Context, e InboundEvent) { gotEvent = e },
			})
			require.NoError(err)

			messageID, err := svc.ArchiveInbound(ctx, InboundMessage{
				Account:    "15551234567@s.whatsapp.net",
				ChatJID:    "15557654321@s.whatsapp.net",
				SenderJID:  "15557654321@s.whatsapp.net",
				MessageID:  "media-2",
				Timestamp:  time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
				Attachment: tt.attachment,
			})
			require.NoError(err, "a failed/unstorable download must not drop the message")
			require.NotZero(messageID)

			// A failed download must not leave the message looking
			// attachment-free: a marker row records that real media was sent
			// even though its bytes were never durably stored.
			row, ok := queryWhatsAppAttachment(t, st, messageID)
			require.True(ok, "a marker row must exist so the failure is visible/queryable")
			assert.Equal("RIB.pdf", row.filename)
			assert.Empty(row.contentHash, "no content hash: bytes were never durably stored")
			assert.Contains(row.storagePath, "whatsapp:pending:")

			hasAttachments, count := queryMessageAttachmentStats(t, st, messageID)
			assert.True(hasAttachments, "message must not look attachment-free")
			assert.Equal(int64(1), count)

			// size_estimate mirrors Gmail/IMAP's "size of the fetched
			// message": it reflects bytes actually downloaded (Data), not
			// whether local CAS storage was configured/succeeded.
			var sizeEstimate int64
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT size_estimate FROM messages WHERE id = ?
			`), messageID).Scan(&sizeEstimate))
			assert.Equal(int64(len(tt.attachment.Data)), sizeEstimate)
			assert.False(gotEvent.HasAttachment, "webhook must not point consumers at bytes that were never stored")
			assert.Empty(gotEvent.AttachmentMediaType)
			assert.Empty(gotEvent.AttachmentFilename)
		})
	}
}

func TestServiceArchiveInboundAttachmentStoreFailureDoesNotFailMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	// A regular file where a directory is expected forces
	// export.StoreAttachmentFile to fail with a genuine store-layer error
	// (mirroring a real misconfigured or unwritable AttachmentsDir), rather
	// than a stubbed one.
	badDir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(os.WriteFile(badDir, []byte("x"), 0o600))

	notifications := 0
	transport := &mockTransport{status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true}}
	svc, err := NewService(ServiceOptions{
		Store:          st,
		Transport:      transport,
		AttachmentsDir: badDir,
		Notify:         func(context.Context, InboundEvent) { notifications++ },
	})
	require.NoError(err)

	messageID, err := svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "15557654321@s.whatsapp.net",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "media-store-fail",
		Timestamp: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
		Attachment: &InboundAttachment{
			Filename:  "RIB.pdf",
			MimeType:  "application/pdf",
			MediaType: "document",
			Data:      []byte("bank details"),
		},
	})
	require.NoError(err, "an attachment store failure must not fail an already-committed message")
	require.NotZero(messageID)
	assert.Equal(1, notifications, "recomputeStats/notify must still run despite the attachment failure")

	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE id = ?`), messageID).Scan(&count))
	assert.Equal(1, count, "the message itself must still be committed")
}

func TestServiceArchiveInboundNotifiesAttachmentMetadataOnSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	var gotEvent InboundEvent
	transport := &mockTransport{status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true}}
	svc, err := NewService(ServiceOptions{
		Store:          st,
		Transport:      transport,
		AttachmentsDir: t.TempDir(),
		Notify:         func(_ context.Context, e InboundEvent) { gotEvent = e },
	})
	require.NoError(err)

	messageID, err := svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "15557654321@s.whatsapp.net",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "media-notify",
		Timestamp: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
		Attachment: &InboundAttachment{
			Filename:  "RIB.pdf",
			MimeType:  "application/pdf",
			MediaType: "document",
			Data:      []byte("bank details"),
		},
	})
	require.NoError(err)
	require.NotZero(messageID)
	assert.True(gotEvent.HasAttachment)
	assert.Equal("document", gotEvent.AttachmentMediaType)
	assert.Equal("RIB.pdf", gotEvent.AttachmentFilename)
	assert.Equal(messageID, gotEvent.StoreMessageID)
}

func TestServiceArchiveInboundCaptionlessAttachmentMessageIsRetrievable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	attachmentsDir := t.TempDir()
	transport := &mockTransport{
		status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true},
	}
	svc, err := NewService(ServiceOptions{
		Store:          st,
		Transport:      transport,
		AttachmentsDir: attachmentsDir,
	})
	require.NoError(err)

	// Regression coverage for the reported bug: a bare document with no
	// caption text used to be silently dropped (MessageText returned "" and
	// convertMessage discarded the event), so nothing was ever archived.
	messageID, err := svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "15557654321@s.whatsapp.net",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "media-3",
		Timestamp: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
		Attachment: &InboundAttachment{
			Filename:  "RIB.pdf",
			MimeType:  "application/pdf",
			MediaType: "document",
			Data:      []byte("bank details"),
		},
	})
	require.NoError(err)
	require.NotZero(messageID)

	var storagePath string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT storage_path FROM attachments WHERE message_id = ?
	`), messageID).Scan(&storagePath))
	assert.NotEmpty(storagePath)
	assert.Equal(filepath.ToSlash(storagePath), storagePath, "storage path uses the CAS convention (hash[:2]/hash)")
}

package live

import (
	"context"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type mockTransport struct {
	status         Status
	connect        func(context.Context) error
	logout         func(context.Context, TransportLogoutRequest) (TransportLogoutResult, error)
	startQRPairing func(context.Context) error
	pairingState   QRPairingState
	sendMessage    func(context.Context, TransportSendMessageRequest) (TransportSendResult, error)
	sendReaction   func(context.Context, TransportSendReactionRequest) (TransportSendResult, error)
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

func TestServiceSendMessageArchivesAndOutbox(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	sentAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	transport := &mockTransport{
		status: Status{
			AccountJID: "15551234567@s.whatsapp.net",
			Paired:     true,
			Connected:  true,
			LoggedIn:   true,
		},
		sendMessage: func(_ context.Context, req TransportSendMessageRequest) (TransportSendResult, error) {
			assertpkg.Equal(t, "15557654321@s.whatsapp.net", req.ChatID)
			assertpkg.Equal(t, "hello", req.Body)
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
	requirepkg.NoError(t, err)

	result, err := svc.SendMessage(ctx, SendMessageRequest{
		ChatID:         "15557654321@s.whatsapp.net",
		Body:           "hello",
		LocalRequestID: "req-1",
	})
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, store.WhatsAppOutboxSent, result.Status)
	assertpkg.Equal(t, "remote-1", result.RemoteMessageID)
	assertpkg.NotZero(t, result.MessageID)

	outbox, err := st.GetWhatsAppOutbox(ctx, result.OutboxID)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, store.WhatsAppOutboxSent, outbox.Status)
	assertpkg.Equal(t, result.MessageID, outbox.MessageID.Int64)

	var body string
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text
		FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.id = ?
	`), result.MessageID).Scan(&body))
	assertpkg.Equal(t, "hello", body)

	ref, err := st.GetWhatsAppMessageRef(ctx, result.MessageID)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, "15557654321@s.whatsapp.net", ref.ChatJID)
	assertpkg.Equal(t, "remote-1", ref.RemoteMessageID)
	assertpkg.True(t, ref.IsFromMe)
}

func TestServiceSendMessageRejectsNotReady(t *testing.T) {
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
	requirepkg.NoError(t, err)

	_, err = svc.SendMessage(ctx, SendMessageRequest{
		ChatID: "15557654321@s.whatsapp.net",
		Body:   "hello",
	})
	requirepkg.Error(t, err)
	assertpkg.Contains(t, err.Error(), "ready=true")
	assertpkg.False(t, called)
}

func TestServiceStartLoginKeepsReconnectContextAlive(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	loginCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	requirepkg.NoError(t, err)

	state, err := svc.StartLogin(ctx)
	requirepkg.NoError(t, err)
	requirepkg.NotNil(t, connectCtx)
	assertpkg.True(t, state.Status.Ready)
	assertpkg.NoError(t, connectCtx.Err())
}

func TestServiceSendReactionUpdatesLocalReaction(t *testing.T) {
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
	requirepkg.NoError(t, err)

	targetID, err := svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "15557654321@s.whatsapp.net",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "target-1",
		Text:      "target",
		Timestamp: now.Add(-time.Minute),
	})
	requirepkg.NoError(t, err)

	result, err := svc.SendReaction(ctx, SendReactionRequest{
		MessageID:      targetID,
		Emoji:          "👍",
		LocalRequestID: "react-1",
	})
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, store.WhatsAppOutboxSent, result.Status)
	assertpkg.Equal(t, "15557654321@s.whatsapp.net", reactionReq.ChatJID)
	assertpkg.Equal(t, "15557654321@s.whatsapp.net", reactionReq.SenderJID)
	assertpkg.Equal(t, "target-1", reactionReq.RemoteMessageID)
	assertpkg.Equal(t, "👍", reactionReq.Emoji)
	outbox, err := st.GetWhatsAppOutbox(ctx, result.OutboxID)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, targetID, outbox.MessageID.Int64)

	selfID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15551234567@s.whatsapp.net", "")
	requirepkg.NoError(t, err)
	var active string
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT reaction_value
		FROM reactions
		WHERE message_id = ? AND participant_id = ? AND removed_at IS NULL
	`), targetID, selfID).Scan(&active))
	assertpkg.Equal(t, "👍", active)
}

func TestServiceArchiveInboundReactionClearsReaction(t *testing.T) {
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
	requirepkg.NoError(t, err)

	targetID, err := svc.ArchiveInbound(ctx, InboundMessage{
		Account:   "15551234567@s.whatsapp.net",
		ChatJID:   "120363@g.us",
		SenderJID: "15557654321@s.whatsapp.net",
		MessageID: "target-1",
		Text:      "target",
		Timestamp: now.Add(-time.Minute),
		IsGroup:   true,
	})
	requirepkg.NoError(t, err)

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
	requirepkg.NoError(t, err)

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
	requirepkg.NoError(t, err)

	participantID, err := st.EnsureParticipantByIdentifier(store.WhatsAppSourceType, "15557654321@s.whatsapp.net", "")
	requirepkg.NoError(t, err)
	var activeCount int
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM reactions
		WHERE message_id = ? AND participant_id = ? AND removed_at IS NULL
	`), targetID, participantID).Scan(&activeCount))
	assertpkg.Equal(t, 0, activeCount)
}

func TestServiceLogoutReturnsBeforeAndAfterStatus(t *testing.T) {
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
	requirepkg.NoError(t, err)

	result, err := svc.Logout(ctx, LogoutRequest{ForceLocal: true})
	requirepkg.NoError(t, err)
	assertpkg.True(t, gotReq.ForceLocal)
	assertpkg.True(t, result.StatusBefore.Paired)
	assertpkg.False(t, result.StatusAfter.Paired)
	assertpkg.True(t, result.RemoteLogout)
	assertpkg.True(t, result.LocalSessionCleared)
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
	requirepkg.NoError(t, err)

	_, err = svc.StartLogin(ctx)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, "daemon", gotScope)
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
	requirepkg.NoError(t, err)

	_, err = svc.StartLogin(ctx)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, "daemon", gotScope)
}

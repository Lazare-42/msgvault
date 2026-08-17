package live

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/export"
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

	duplicate, err := svc.SendMessage(ctx, SendMessageRequest{
		ChatID:         "15557654321@s.whatsapp.net",
		Body:           "hello",
		LocalRequestID: "req-1",
	})
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, result, duplicate)
	assertpkg.Equal(t, 1, sendCalls)

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

func TestServiceArchiveHistorySyncBatchesStatsAndSkipsNotifications(t *testing.T) {
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
	requirepkg.NoError(t, err)

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
	requirepkg.NoError(t, svc.ArchiveHistorySync(ctx, messages))
	requirepkg.NoError(t, svc.ArchiveHistorySync(ctx, messages), "history sync retry is idempotent")
	assertpkg.Zero(t, notifications)

	var title string
	var messageCount int64
	var preview string
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT title, message_count, last_message_preview
		FROM conversations
		WHERE source_conversation_id = ?
	`), "120363@g.us").Scan(&title, &messageCount, &preview))
	assertpkg.Equal(t, "Beynac Team", title)
	assertpkg.Equal(t, int64(2), messageCount)
	assertpkg.Equal(t, "second", preview)
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
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT has_attachments, attachment_count FROM messages WHERE id = ?
	`), messageID).Scan(&hasAttachments, &count))
	return hasAttachments, count
}

func TestServiceArchiveInboundStoresDownloadedAttachment(t *testing.T) {
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
	requirepkg.NoError(t, err)

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
	requirepkg.NoError(t, err, "a captionless attachment message must still archive")
	requirepkg.NotZero(t, messageID)

	row, ok := queryWhatsAppAttachment(t, st, messageID)
	requirepkg.True(t, ok, "attachment row must exist for downloaded media")
	assertpkg.Equal(t, "RIB.pdf", row.filename)
	assertpkg.Equal(t, "application/pdf", row.mimeType)
	assertpkg.Equal(t, "document", row.mediaType)
	assertpkg.Equal(t, int64(len(content)), row.size)
	assertpkg.NotEmpty(t, row.contentHash)

	storedPath, err := export.StoragePath(attachmentsDir, row.contentHash)
	requirepkg.NoError(t, err)
	stored, err := os.ReadFile(storedPath)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, content, stored)

	hasAttachments, count := queryMessageAttachmentStats(t, st, messageID)
	assertpkg.True(t, hasAttachments)
	assertpkg.Equal(t, int64(1), count)

	var sizeEstimate int64
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT size_estimate FROM messages WHERE id = ?
	`), messageID).Scan(&sizeEstimate))
	assertpkg.Equal(t, int64(len(content)), sizeEstimate, "size_estimate must include downloaded attachment bytes")
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
			requirepkg.NoError(t, err)

			messageID, err := svc.ArchiveInbound(ctx, InboundMessage{
				Account:    "15551234567@s.whatsapp.net",
				ChatJID:    "15557654321@s.whatsapp.net",
				SenderJID:  "15557654321@s.whatsapp.net",
				MessageID:  "media-2",
				Timestamp:  time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
				Attachment: tt.attachment,
			})
			requirepkg.NoError(t, err, "a failed/unstorable download must not drop the message")
			requirepkg.NotZero(t, messageID)

			// A failed download must not leave the message looking
			// attachment-free: a marker row records that real media was sent
			// even though its bytes were never durably stored.
			row, ok := queryWhatsAppAttachment(t, st, messageID)
			requirepkg.True(t, ok, "a marker row must exist so the failure is visible/queryable")
			assertpkg.Equal(t, "RIB.pdf", row.filename)
			assertpkg.Empty(t, row.contentHash, "no content hash: bytes were never durably stored")
			assertpkg.Contains(t, row.storagePath, "whatsapp:pending:")

			hasAttachments, count := queryMessageAttachmentStats(t, st, messageID)
			assertpkg.True(t, hasAttachments, "message must not look attachment-free")
			assertpkg.Equal(t, int64(1), count)

			// size_estimate mirrors Gmail/IMAP's "size of the fetched
			// message": it reflects bytes actually downloaded (Data), not
			// whether local CAS storage was configured/succeeded.
			var sizeEstimate int64
			requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
				SELECT size_estimate FROM messages WHERE id = ?
			`), messageID).Scan(&sizeEstimate))
			assertpkg.Equal(t, int64(len(tt.attachment.Data)), sizeEstimate)

			assertpkg.False(t, gotEvent.HasAttachment, "webhook must not point consumers at bytes that were never stored")
			assertpkg.Empty(t, gotEvent.AttachmentMediaType)
			assertpkg.Empty(t, gotEvent.AttachmentFilename)
		})
	}
}

func TestServiceArchiveInboundAttachmentStoreFailureDoesNotFailMessage(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	// A regular file where a directory is expected forces
	// export.StoreAttachmentFile to fail with a genuine store-layer error
	// (mirroring a real misconfigured or unwritable AttachmentsDir), rather
	// than a stubbed one.
	badDir := filepath.Join(t.TempDir(), "not-a-dir")
	requirepkg.NoError(t, os.WriteFile(badDir, []byte("x"), 0o600))

	notifications := 0
	transport := &mockTransport{status: Status{AccountJID: "15551234567@s.whatsapp.net", Paired: true}}
	svc, err := NewService(ServiceOptions{
		Store:          st,
		Transport:      transport,
		AttachmentsDir: badDir,
		Notify:         func(context.Context, InboundEvent) { notifications++ },
	})
	requirepkg.NoError(t, err)

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
	requirepkg.NoError(t, err, "an attachment store failure must not fail an already-committed message")
	requirepkg.NotZero(t, messageID)
	assertpkg.Equal(t, 1, notifications, "recomputeStats/notify must still run despite the attachment failure")

	var count int
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE id = ?`), messageID).Scan(&count))
	assertpkg.Equal(t, 1, count, "the message itself must still be committed")
}

func TestServiceArchiveInboundNotifiesAttachmentMetadataOnSuccess(t *testing.T) {
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
	requirepkg.NoError(t, err)

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
	requirepkg.NoError(t, err)
	requirepkg.NotZero(t, messageID)

	assertpkg.True(t, gotEvent.HasAttachment)
	assertpkg.Equal(t, "document", gotEvent.AttachmentMediaType)
	assertpkg.Equal(t, "RIB.pdf", gotEvent.AttachmentFilename)
	assertpkg.Equal(t, messageID, gotEvent.StoreMessageID)
}

func TestServiceArchiveInboundCaptionlessAttachmentMessageIsRetrievable(t *testing.T) {
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
	requirepkg.NoError(t, err)

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
	requirepkg.NoError(t, err)
	requirepkg.NotZero(t, messageID)

	var storagePath string
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT storage_path FROM attachments WHERE message_id = ?
	`), messageID).Scan(&storagePath))
	assertpkg.NotEmpty(t, storagePath)
	assertpkg.Equal(t, filepath.ToSlash(storagePath), storagePath, "storage path uses the CAS convention (hash[:2]/hash)")
}

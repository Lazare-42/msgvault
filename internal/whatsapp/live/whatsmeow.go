package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdmime "mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	_ "github.com/mattn/go-sqlite3"
)

// mediaDownloadTimeout bounds a single media download attempt. WhatsApp CDN
// URLs and media keys expire, so a hung or slow download must not stall the
// live event handler indefinitely.
const mediaDownloadTimeout = 60 * time.Second

// mediaDownloader is the subset of *whatsmeow.Client used to fetch the bytes
// referenced by a downloadable media message. It exists so tests can supply a
// fake without standing up a real whatsmeow client/session.
type mediaDownloader interface {
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
}

type WhatsmeowOptions struct {
	SessionPath string
	Account     string
	LogLevel    string
}

type WhatsmeowTransport struct {
	client      *whatsmeow.Client
	container   *sqlstore.Container
	sessionPath string
	account     string
	log         waLog.Logger
	downloader  mediaDownloader

	mu            sync.Mutex
	handlerID     uint32
	inbound       func(context.Context, InboundMessage) error
	historySync   func(context.Context, []InboundMessage) error
	pairingCancel context.CancelFunc
	pairingState  QRPairingState
}

func NewWhatsmeowTransport(ctx context.Context, opts WhatsmeowOptions) (*WhatsmeowTransport, error) {
	if opts.SessionPath == "" {
		return nil, errors.New("session path is required")
	}
	if err := ensureSessionFile(opts.SessionPath); err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     opts.SessionPath,
		RawQuery: "_foreign_keys=on&_busy_timeout=30000",
	}
	log := newWhatsmeowLogger(opts.LogLevel)
	container, err := sqlstore.New(ctx, "sqlite3", u.String(), log.Sub("SQLStore"))
	if err != nil {
		return nil, fmt.Errorf("open whatsapp session: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("get whatsapp device: %w", err)
	}
	client := whatsmeow.NewClient(device, log)
	client.EnableAutoReconnect = true
	return &WhatsmeowTransport{
		client:      client,
		container:   container,
		sessionPath: opts.SessionPath,
		account:     strings.TrimSpace(opts.Account),
		log:         log,
		downloader:  client,
	}, nil
}

func newWhatsmeowLogger(level string) waLog.Logger {
	level = strings.TrimSpace(level)
	if level == "" {
		level = "INFO"
	}
	return waLog.Stdout("WhatsApp", level, false)
}

func (t *WhatsmeowTransport) SetInboundHandler(handler func(context.Context, InboundMessage) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inbound = handler
}

func (t *WhatsmeowTransport) SetHistorySyncHandler(handler func(context.Context, []InboundMessage) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.historySync = handler
}

func (t *WhatsmeowTransport) Status(ctx context.Context) (Status, error) {
	_ = ctx
	accountJID := ""
	if t.client != nil && t.client.Store != nil && t.client.Store.ID != nil {
		accountJID = t.client.Store.ID.ToNonAD().String()
	}
	account := t.account
	if account == "" {
		account = accountJID
	}
	status := Status{
		Account:     account,
		AccountJID:  accountJID,
		Connected:   t.client != nil && t.client.IsConnected(),
		LoggedIn:    t.client != nil && t.client.IsLoggedIn(),
		Paired:      accountJID != "",
		SessionPath: t.sessionPath,
	}
	status.ApplyDerived()
	return status, nil
}

func (t *WhatsmeowTransport) Connect(ctx context.Context) error {
	t.registerEventHandler(ctx)
	return t.client.ConnectContext(ctx)
}

func (t *WhatsmeowTransport) Logout(ctx context.Context, req TransportLogoutRequest) (TransportLogoutResult, error) {
	t.cancelPairing()
	status, err := t.Status(ctx)
	if err != nil {
		return TransportLogoutResult{}, err
	}
	result := TransportLogoutResult{}
	if !status.Paired {
		if !req.ForceLocal {
			return result, nil
		}
		result.ForcedLocalClear = true
		if err := t.clearLocalSession(ctx); err != nil {
			return result, fmt.Errorf("clear local session: %w", err)
		}
		result.LocalSessionCleared = true
		return result, nil
	}

	if !status.Connected {
		t.registerEventHandler(ctx)
		if err := t.client.ConnectContext(ctx); err != nil {
			return t.finishLogoutFailure(ctx, result, req, fmt.Errorf("connect before logout: %w", err))
		}
	}
	if err := t.client.Logout(ctx); err != nil {
		return t.finishLogoutFailure(ctx, result, req, fmt.Errorf("remote logout: %w", err))
	}
	result.RemoteLogout = true
	result.LocalSessionCleared = true
	if err := t.resetClient(ctx); err != nil {
		return result, fmt.Errorf("reset whatsapp client after logout: %w", err)
	}
	return result, nil
}

func (t *WhatsmeowTransport) finishLogoutFailure(ctx context.Context, result TransportLogoutResult, req TransportLogoutRequest, logoutErr error) (TransportLogoutResult, error) {
	if !req.ForceLocal {
		return result, logoutErr
	}
	result.ForcedLocalClear = true
	if err := t.clearLocalSession(ctx); err != nil {
		return result, errors.Join(logoutErr, fmt.Errorf("clear local session: %w", err))
	}
	result.LocalSessionCleared = true
	return result, nil
}

func (t *WhatsmeowTransport) clearLocalSession(ctx context.Context) error {
	if t.client != nil {
		t.client.Disconnect()
		if t.client.Store != nil && t.client.Store.ID != nil {
			if err := t.client.Store.Delete(ctx); err != nil {
				return err
			}
		}
	}
	return t.resetClient(ctx)
}

func (t *WhatsmeowTransport) resetClient(ctx context.Context) error {
	device, err := t.container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("get fresh whatsapp device: %w", err)
	}
	client := whatsmeow.NewClient(device, t.log)
	client.EnableAutoReconnect = true

	t.mu.Lock()
	oldClient := t.client
	oldHandlerID := t.handlerID
	if t.pairingCancel != nil {
		t.pairingCancel()
		t.pairingCancel = nil
	}
	t.client = client
	t.downloader = client
	t.handlerID = 0
	t.pairingState = QRPairingState{}
	t.mu.Unlock()

	if oldClient != nil && oldHandlerID != 0 {
		oldClient.RemoveEventHandler(oldHandlerID)
	}
	return nil
}

func (t *WhatsmeowTransport) cancelPairing() {
	t.mu.Lock()
	if t.pairingCancel != nil {
		t.pairingCancel()
		t.pairingCancel = nil
	}
	t.pairingState = QRPairingState{}
	t.mu.Unlock()
}

func (t *WhatsmeowTransport) Close() error {
	t.mu.Lock()
	if t.pairingCancel != nil {
		t.pairingCancel()
		t.pairingCancel = nil
	}
	if t.handlerID != 0 {
		t.client.RemoveEventHandler(t.handlerID)
		t.handlerID = 0
	}
	t.mu.Unlock()
	if t.client != nil {
		t.client.Disconnect()
	}
	if t.container != nil {
		return t.container.Close()
	}
	return nil
}

func (t *WhatsmeowTransport) StartQRPairing(ctx context.Context) error {
	status, err := t.Status(ctx)
	if err != nil {
		return err
	}
	if status.Paired {
		t.setPairingState(QRPairingState{
			Event:  whatsmeow.QRChannelSuccess.Event,
			Paired: true,
		})
		return nil
	}

	t.mu.Lock()
	if t.pairingState.Active {
		t.mu.Unlock()
		return nil
	}
	pairingCtx, cancel := context.WithCancel(ctx)
	t.pairingCancel = cancel
	t.pairingState = QRPairingState{
		Active: true,
		Event:  "starting",
	}
	t.mu.Unlock()

	go t.runQRPairing(pairingCtx, ctx)
	return nil
}

func (t *WhatsmeowTransport) PairingState(ctx context.Context) (QRPairingState, error) {
	status, err := t.Status(ctx)
	if err != nil {
		return QRPairingState{}, err
	}
	t.mu.Lock()
	state := t.pairingState
	t.mu.Unlock()
	state.Paired = state.Paired || status.Paired
	if state.Paired {
		state.Active = false
		state.Code = ""
		state.ExpiresAt = time.Time{}
		if state.Event == "" {
			state.Event = whatsmeow.QRChannelSuccess.Event
		}
	}
	return state, nil
}

func (t *WhatsmeowTransport) runQRPairing(pairingCtx context.Context, handlerCtx context.Context) {
	qrChan, err := t.client.GetQRChannel(pairingCtx)
	if err != nil {
		t.finishQRPairing(QRPairingState{Event: "error", Error: err.Error()})
		return
	}
	t.registerEventHandler(handlerCtx)
	if err := t.client.ConnectContext(pairingCtx); err != nil {
		t.finishQRPairing(QRPairingState{Event: "error", Error: err.Error()})
		return
	}
	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			t.setPairingState(QRPairingState{
				Active:    true,
				Code:      item.Code,
				Event:     item.Event,
				ExpiresAt: time.Now().Add(item.Timeout),
			})
		case whatsmeow.QRChannelEventError:
			errMsg := "unknown QR pairing error"
			if item.Error != nil {
				errMsg = item.Error.Error()
			}
			t.finishQRPairing(QRPairingState{Event: item.Event, Error: errMsg})
			return
		case whatsmeow.QRChannelSuccess.Event:
			t.finishQRPairing(QRPairingState{Event: item.Event, Paired: true})
			return
		default:
			t.finishQRPairing(QRPairingState{
				Event: item.Event,
				Error: fmt.Sprintf("whatsapp pairing ended: %s", item.Event),
			})
			return
		}
	}
	t.finishQRPairing(QRPairingState{
		Event: "closed",
		Error: "whatsapp QR channel closed before pairing completed",
	})
}

func (t *WhatsmeowTransport) setPairingState(state QRPairingState) {
	t.mu.Lock()
	t.pairingState = state
	t.mu.Unlock()
}

func (t *WhatsmeowTransport) finishQRPairing(state QRPairingState) {
	state.Active = false
	state.Code = ""
	state.ExpiresAt = time.Time{}
	t.mu.Lock()
	if t.pairingCancel != nil {
		t.pairingCancel()
		t.pairingCancel = nil
	}
	t.pairingState = state
	t.mu.Unlock()
}

func (t *WhatsmeowTransport) SendMessage(ctx context.Context, req TransportSendMessageRequest) (TransportSendResult, error) {
	chat, err := ParseChatJID(req.ChatID)
	if err != nil {
		return TransportSendResult{}, err
	}
	var msg *waE2E.Message
	if len(req.Mentions) > 0 {
		// @mentions require an ExtendedTextMessage carrying the mentioned JIDs.
		// WhatsApp then renders each "@<user>" token in Text as the contact's
		// name and pings them (group participants resolve via pushname/LID).
		msg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(req.Body),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: req.Mentions,
				},
			},
		}
	} else {
		msg = &waE2E.Message{Conversation: proto.String(req.Body)}
	}
	resp, err := t.client.SendMessage(ctx, chat, msg)
	if err != nil {
		return TransportSendResult{}, err
	}
	return TransportSendResult{
		RemoteMessageID: string(resp.ID),
		ChatJID:         chat.ToNonAD().String(),
		Timestamp:       resp.Timestamp,
	}, nil
}

func (t *WhatsmeowTransport) SendReaction(ctx context.Context, req TransportSendReactionRequest) (TransportSendResult, error) {
	chat, err := ParseChatJID(req.ChatJID)
	if err != nil {
		return TransportSendResult{}, err
	}
	sender, err := ParseChatJID(req.SenderJID)
	if err != nil {
		return TransportSendResult{}, fmt.Errorf("sender_jid: %w", err)
	}
	msg := t.client.BuildReaction(chat, sender, types.MessageID(req.RemoteMessageID), req.Emoji)
	resp, err := t.client.SendMessage(ctx, chat, msg)
	if err != nil {
		return TransportSendResult{}, err
	}
	return TransportSendResult{
		RemoteMessageID: string(resp.ID),
		ChatJID:         chat.ToNonAD().String(),
		Timestamp:       resp.Timestamp,
	}, nil
}

func (t *WhatsmeowTransport) LinkQR(ctx context.Context, w io.Writer) error {
	qrChan, err := t.client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := t.client.ConnectContext(ctx); err != nil {
		return err
	}
	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			_, _ = fmt.Fprintf(w, "QR code payload:\n%s\n\nExpires in %s\n", item.Code, item.Timeout.Round(time.Second))
		case whatsmeow.QRChannelEventError:
			return item.Error
		case whatsmeow.QRChannelSuccess.Event:
			_, _ = fmt.Fprintln(w, "WhatsApp linked")
			return nil
		default:
			return fmt.Errorf("whatsapp pairing ended: %s", item.Event)
		}
	}
	return errors.New("whatsapp QR channel closed before pairing completed")
}

func (t *WhatsmeowTransport) PairPhone(ctx context.Context, phone string, w io.Writer) error {
	qrChan, err := t.client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := t.client.ConnectContext(ctx); err != nil {
		return err
	}
	select {
	case item := <-qrChan:
		if item.Event != whatsmeow.QRChannelEventCode {
			return fmt.Errorf("expected QR setup event, got %s", item.Event)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	code, err := t.client.PairPhone(ctx, phone, false, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "WhatsApp pairing code: %s\n", code)
	return nil
}

func (t *WhatsmeowTransport) registerEventHandler(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handlerID != 0 {
		return
	}
	t.handlerID = t.client.AddEventHandler(func(evt any) {
		t.logWhatsAppEvent(evt)

		switch event := evt.(type) {
		case *events.Message:
			if _, err := t.convertAndArchiveMessage(ctx, event, ""); err != nil {
				t.client.Log.Warnf("Failed to archive WhatsApp message %s: %v", event.Info.ID, err)
			}
		case *events.HistorySync:
			go t.archiveHistorySync(ctx, event)
		}
	})
}

func (t *WhatsmeowTransport) convertAndArchiveMessage(ctx context.Context, msg *events.Message, chatTitle string) (bool, error) {
	inbound, ok := t.convertMessage(ctx, msg)
	if !ok {
		return false, nil
	}
	inbound.ChatTitle = strings.TrimSpace(chatTitle)
	t.mu.Lock()
	handler := t.inbound
	t.mu.Unlock()
	if handler == nil {
		return false, nil
	}
	return true, handler(ctx, inbound)
}

func (t *WhatsmeowTransport) archiveHistorySync(ctx context.Context, event *events.HistorySync) {
	if event == nil || event.Data == nil {
		return
	}
	conversations := event.Data.GetConversations()
	t.client.Log.Infof("WhatsApp history sync received: type=%s conversations=%d progress=%d", event.Data.GetSyncType().String(), len(conversations), event.Data.GetProgress())

	messages := make([]InboundMessage, 0)
	parseFailures := 0
	for _, conversation := range conversations {
		if err := ctx.Err(); err != nil {
			return
		}
		chatJID, err := types.ParseJID(conversation.GetID())
		if err != nil {
			parseFailures++
			continue
		}
		title := strings.TrimSpace(conversation.GetDisplayName())
		if title == "" {
			title = strings.TrimSpace(conversation.GetName())
		}
		for _, historyMessage := range conversation.GetMessages() {
			parsed, err := t.client.ParseWebMessage(chatJID, historyMessage.GetMessage())
			if err != nil {
				parseFailures++
				continue
			}
			// History-sync messages parse into the same *waE2E.Message shape as
			// live events (ParseWebMessage sets RawMessage = webMsg.GetMessage()),
			// including the per-attachment MediaKey/DirectPath fields, so the same
			// download path applies. In practice many of these URLs will have
			// expired by the time history sync runs; that surfaces as a per-item
			// DownloadError rather than a structural difference to special-case.
			inbound, ok := t.convertMessage(ctx, parsed)
			if !ok {
				continue
			}
			inbound.ChatTitle = title
			messages = append(messages, inbound)
		}
	}

	t.mu.Lock()
	handler := t.historySync
	t.mu.Unlock()
	if handler == nil || len(messages) == 0 {
		t.client.Log.Infof("WhatsApp history sync skipped: messages=%d parse_failures=%d handler=%t", len(messages), parseFailures, handler != nil)
		return
	}
	if err := handler(ctx, messages); err != nil {
		t.client.Log.Warnf("WhatsApp history sync archive completed with errors: messages=%d parse_failures=%d error=%v", len(messages), parseFailures, err)
		return
	}
	t.client.Log.Infof("WhatsApp history sync archived: messages=%d parse_failures=%d", len(messages), parseFailures)
}

func (t *WhatsmeowTransport) logWhatsAppEvent(evt any) {
	if t.client == nil || t.client.Log == nil {
		return
	}
	switch event := evt.(type) {
	case *events.PairSuccess:
		t.client.Log.Infof("WhatsApp pair success: jid=%s lid=%s platform=%s business_name=%q", event.ID, event.LID, event.Platform, event.BusinessName)
	case *events.PairError:
		t.client.Log.Errorf("WhatsApp pair error: jid=%s lid=%s platform=%s business_name=%q error=%v", event.ID, event.LID, event.Platform, event.BusinessName, event.Error)
	case *events.PairPasskeyRequest:
		t.client.Log.Infof("WhatsApp pair passkey requested")
	case *events.PairPasskeyError:
		t.client.Log.Errorf("WhatsApp pair passkey error: continuation=%t error=%v", event.Continuation, event.Error)
	case *events.PairPasskeyConfirmation:
		t.client.Log.Infof("WhatsApp pair passkey confirmation received: skip_handoff_ux=%t", event.SkipHandoffUX)
	case *events.QRScannedWithoutMultidevice:
		t.client.Log.Warnf("WhatsApp QR scanned without multidevice enabled")
	case *events.Connected:
		t.client.Log.Infof("WhatsApp connected and authenticated")
	case *events.Disconnected:
		t.client.Log.Warnf("WhatsApp disconnected")
	case *events.LoggedOut:
		t.client.Log.Warnf("WhatsApp logged out: on_connect=%t reason=%s", event.OnConnect, event.Reason.String())
	case *events.StreamReplaced:
		t.client.Log.Warnf("WhatsApp stream replaced by another client")
	case *events.ClientOutdated:
		t.client.Log.Errorf("WhatsApp client outdated")
	case *events.ConnectFailure:
		t.client.Log.Errorf("WhatsApp connect failure: reason=%s message=%q", event.Reason.String(), event.Message)
	case *events.TemporaryBan:
		t.client.Log.Errorf("WhatsApp temporary ban: %s", event.String())
	case *events.CATRefreshError:
		t.client.Log.Errorf("WhatsApp CAT refresh error: %v", event.Error)
	case *events.StreamError:
		t.client.Log.Errorf("WhatsApp stream error: code=%s", event.Code)
	case *events.KeepAliveTimeout:
		t.client.Log.Warnf("WhatsApp keepalive timeout: error_count=%d last_success=%s", event.ErrorCount, event.LastSuccess.Format(time.RFC3339))
	case *events.KeepAliveRestored:
		t.client.Log.Infof("WhatsApp keepalive restored")
	}
}

func (t *WhatsmeowTransport) convertMessage(ctx context.Context, evt *events.Message) (InboundMessage, bool) {
	if evt == nil || evt.Message == nil || evt.Info.ID == "" {
		return InboundMessage{}, false
	}
	raw, _ := protojson.Marshal(evt.Message)
	chat := evt.Info.Chat.ToNonAD().String()
	sender := evt.Info.Sender.ToNonAD().String()
	if evt.Info.IsFromMe && sender == "" {
		status, _ := t.Status(context.Background())
		sender = status.AccountJID
	}
	if reaction := evt.Message.GetReactionMessage(); reaction != nil {
		key := reaction.GetKey()
		targetChat := key.GetRemoteJID()
		if targetChat == "" {
			targetChat = chat
		}
		return InboundMessage{
			Account:   t.account,
			ChatJID:   chat,
			SenderJID: sender,
			MessageID: string(evt.Info.ID),
			PushName:  evt.Info.PushName,
			Timestamp: evt.Info.Timestamp,
			IsFromMe:  evt.Info.IsFromMe,
			IsGroup:   evt.Info.IsGroup,
			RawJSON:   raw,
			Reaction: &InboundReaction{
				TargetChatJID:   targetChat,
				TargetMessageID: key.GetID(),
				TargetSenderJID: key.GetParticipant(),
				Emoji:           reaction.GetText(),
				TargetFromMe:    key.GetFromMe(),
			},
		}, true
	}
	text := MessageText(evt.Message)
	attachment := t.downloadMediaAttachment(ctx, evt.Message)
	// A media message with no caption (e.g. a bare PDF or photo) must still be
	// archived: only drop the event when there is neither text nor media.
	if text == "" && attachment == nil {
		return InboundMessage{}, false
	}
	return InboundMessage{
		Account:    t.account,
		ChatJID:    chat,
		SenderJID:  sender,
		MessageID:  string(evt.Info.ID),
		PushName:   evt.Info.PushName,
		Text:       text,
		Timestamp:  evt.Info.Timestamp,
		IsFromMe:   evt.Info.IsFromMe,
		IsGroup:    evt.Info.IsGroup,
		RawJSON:    raw,
		Attachment: attachment,
	}, true
}

// downloadMediaAttachment extracts and downloads the media payload (if any)
// referenced by msg. It returns nil when the message carries no downloadable
// media. A non-nil result with empty Data means a media payload was present
// but the bytes could not be fetched (e.g. an expired media key or a network
// error) — the caller archives the message anyway with whatever metadata was
// available; the failure is logged here and is not retried.
func (t *WhatsmeowTransport) downloadMediaAttachment(ctx context.Context, msg *waE2E.Message) *InboundAttachment {
	downloadable, meta, ok := extractMediaMessage(msg)
	if !ok {
		return nil
	}
	attachment := &InboundAttachment{
		Filename:  meta.filename,
		MimeType:  meta.mimetype,
		MediaType: meta.mediaType,
		Size:      meta.size,
	}
	if t.downloader == nil {
		attachment.DownloadError = "no whatsapp client available for media download"
		return attachment
	}
	downloadCtx, cancel := context.WithTimeout(ctx, mediaDownloadTimeout)
	defer cancel()
	data, err := t.downloader.Download(downloadCtx, downloadable)
	if err != nil {
		attachment.DownloadError = err.Error()
		if t.log != nil {
			t.log.Warnf("WhatsApp media download failed (%s, %s): %v", meta.mediaType, meta.filename, err)
		}
		return attachment
	}
	attachment.Data = data
	attachment.Size = int64(len(data))
	return attachment
}

// waMediaMeta is the caption-independent metadata whatsmeow exposes for a
// downloadable media message, ahead of (and regardless of) the actual download.
type waMediaMeta struct {
	filename  string
	mimetype  string
	mediaType string
	size      int64
}

// extractMediaMessage finds the downloadable media sub-message (if any) on
// msg and returns it along with its metadata. Only one of Image/Video/
// Document/Audio/Sticker is expected to be set per message.
func extractMediaMessage(msg *waE2E.Message) (whatsmeow.DownloadableMessage, waMediaMeta, bool) {
	if msg == nil {
		return nil, waMediaMeta{}, false
	}
	switch {
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		return m, waMediaMeta{
			filename:  mediaFilename("", m.GetMimetype(), "image"),
			mimetype:  m.GetMimetype(),
			mediaType: "image",
			size:      int64(m.GetFileLength()),
		}, true
	case msg.GetVideoMessage() != nil:
		m := msg.GetVideoMessage()
		return m, waMediaMeta{
			filename:  mediaFilename("", m.GetMimetype(), "video"),
			mimetype:  m.GetMimetype(),
			mediaType: "video",
			size:      int64(m.GetFileLength()),
		}, true
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		filename := strings.TrimSpace(m.GetFileName())
		if filename == "" {
			filename = mediaFilename(m.GetTitle(), m.GetMimetype(), "document")
		}
		return m, waMediaMeta{
			filename:  filename,
			mimetype:  m.GetMimetype(),
			mediaType: "document",
			size:      int64(m.GetFileLength()),
		}, true
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		mediaType := "audio"
		if m.GetPTT() {
			mediaType = "voice_note"
		}
		return m, waMediaMeta{
			filename:  mediaFilename("", m.GetMimetype(), mediaType),
			mimetype:  m.GetMimetype(),
			mediaType: mediaType,
			size:      int64(m.GetFileLength()),
		}, true
	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		return m, waMediaMeta{
			filename:  mediaFilename("", m.GetMimetype(), "sticker"),
			mimetype:  m.GetMimetype(),
			mediaType: "sticker",
			size:      int64(m.GetFileLength()),
		}, true
	default:
		return nil, waMediaMeta{}, false
	}
}

// mediaFilename derives a reasonable filename for a media payload that has no
// filename of its own (WhatsApp only sends one for document messages). base,
// when non-empty, is used as the name stem (e.g. a document's title);
// otherwise kind (e.g. "image") is used. The mimetype's registered extension
// is appended when it is not already present.
func mediaFilename(base, mimetype, kind string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = kind
	}
	if ext := extensionForMimetype(mimetype); ext != "" && !strings.HasSuffix(strings.ToLower(base), ext) {
		base += ext
	}
	return base
}

func extensionForMimetype(mimetype string) string {
	mimetype = strings.TrimSpace(strings.SplitN(mimetype, ";", 2)[0])
	if mimetype == "" {
		return ""
	}
	exts, err := stdmime.ExtensionsByType(mimetype)
	if err != nil || len(exts) == 0 {
		return ""
	}
	return exts[0]
}

func MessageText(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation()
	case msg.GetExtendedTextMessage().GetText() != "":
		return msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage().GetCaption() != "":
		return msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage().GetCaption() != "":
		return msg.GetVideoMessage().GetCaption()
	case msg.GetDocumentMessage().GetCaption() != "":
		return msg.GetDocumentMessage().GetCaption()
	case msg.GetLiveLocationMessage().GetCaption() != "":
		return msg.GetLiveLocationMessage().GetCaption()
	case msg.GetLocationMessage().GetName() != "":
		return msg.GetLocationMessage().GetName()
	case msg.GetLocationMessage().GetAddress() != "":
		return msg.GetLocationMessage().GetAddress()
	case msg.GetContactMessage().GetDisplayName() != "":
		return msg.GetContactMessage().GetDisplayName()
	default:
		return ""
	}
}

func ParseChatJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.JID{}, errors.New("jid is required")
	}
	if strings.Contains(raw, "@") {
		jid, err := types.ParseJID(raw)
		if err != nil {
			return types.JID{}, err
		}
		return jid.ToNonAD(), nil
	}
	digits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, raw)
	if digits == "" {
		return types.JID{}, fmt.Errorf("invalid WhatsApp recipient %q", raw)
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

func ensureSessionFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create whatsapp session dir: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("chmod whatsapp session dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("create whatsapp session db: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close whatsapp session db: %w", err)
	}
	return os.Chmod(path, 0600)
}

func RawMessageJSON(msg InboundMessage) []byte {
	raw, _ := json.Marshal(msg)
	return raw
}

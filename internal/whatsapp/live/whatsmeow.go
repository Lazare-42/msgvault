package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	_ "github.com/mattn/go-sqlite3"
)

type WhatsmeowOptions struct {
	SessionPath string
	Account     string
}

type WhatsmeowTransport struct {
	client      *whatsmeow.Client
	container   *sqlstore.Container
	sessionPath string
	account     string

	mu        sync.Mutex
	handlerID uint32
	inbound   func(context.Context, InboundMessage) error
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
	container, err := sqlstore.New(ctx, "sqlite3", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("open whatsapp session: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("get whatsapp device: %w", err)
	}
	client := whatsmeow.NewClient(device, nil)
	client.EnableAutoReconnect = true
	return &WhatsmeowTransport{
		client:      client,
		container:   container,
		sessionPath: opts.SessionPath,
		account:     strings.TrimSpace(opts.Account),
	}, nil
}

func (t *WhatsmeowTransport) SetInboundHandler(handler func(context.Context, InboundMessage) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inbound = handler
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
	return Status{
		Account:     account,
		AccountJID:  accountJID,
		Connected:   t.client != nil && t.client.IsConnected(),
		LoggedIn:    t.client != nil && t.client.IsLoggedIn(),
		Paired:      accountJID != "",
		SessionPath: t.sessionPath,
	}, nil
}

func (t *WhatsmeowTransport) Connect(ctx context.Context) error {
	t.registerEventHandler(ctx)
	return t.client.ConnectContext(ctx)
}

func (t *WhatsmeowTransport) Close() error {
	t.mu.Lock()
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

func (t *WhatsmeowTransport) SendMessage(ctx context.Context, req TransportSendMessageRequest) (TransportSendResult, error) {
	chat, err := ParseChatJID(req.ChatID)
	if err != nil {
		return TransportSendResult{}, err
	}
	resp, err := t.client.SendMessage(ctx, chat, &waE2E.Message{
		Conversation: proto.String(req.Body),
	})
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
		msg, ok := evt.(*events.Message)
		if !ok {
			return
		}
		inbound, ok := t.convertMessage(msg)
		if !ok {
			return
		}
		t.mu.Lock()
		handler := t.inbound
		t.mu.Unlock()
		if handler == nil {
			return
		}
		if err := handler(ctx, inbound); err != nil {
			t.client.Log.Warnf("Failed to archive WhatsApp message %s: %v", msg.Info.ID, err)
		}
	})
}

func (t *WhatsmeowTransport) convertMessage(evt *events.Message) (InboundMessage, bool) {
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
	if text == "" {
		return InboundMessage{}, false
	}
	return InboundMessage{
		Account:   t.account,
		ChatJID:   chat,
		SenderJID: sender,
		MessageID: string(evt.Info.ID),
		PushName:  evt.Info.PushName,
		Text:      text,
		Timestamp: evt.Info.Timestamp,
		IsFromMe:  evt.Info.IsFromMe,
		IsGroup:   evt.Info.IsGroup,
		RawJSON:   raw,
	}, true
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

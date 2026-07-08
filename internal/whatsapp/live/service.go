package live

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/store"
)

const reactionTypeEmoji = "emoji"

type Service struct {
	store     *store.Store
	transport Transport
	account   string
	now       func() time.Time
}

type ServiceOptions struct {
	Store     *store.Store
	Transport Transport
	Account   string
	Now       func() time.Time
}

func NewService(opts ServiceOptions) (*Service, error) {
	if opts.Store == nil {
		return nil, errors.New("store is required")
	}
	if opts.Transport == nil {
		return nil, errors.New("transport is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:     opts.Store,
		transport: opts.Transport,
		account:   strings.TrimSpace(opts.Account),
		now:       now,
	}, nil
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	status, err := s.transport.Status(ctx)
	if err != nil {
		return Status{}, err
	}
	if status.Account == "" {
		status.Account = s.account
	}
	return status, nil
}

func (s *Service) Connect(ctx context.Context) error {
	return s.transport.Connect(ctx)
}

func (s *Service) Close() error {
	return s.transport.Close()
}

func (s *Service) SendMessage(ctx context.Context, req SendMessageRequest) (SendResult, error) {
	req.ChatID = strings.TrimSpace(req.ChatID)
	req.Body = strings.TrimSpace(req.Body)
	if req.ChatID == "" {
		return SendResult{}, errors.New("chat_id is required")
	}
	if req.Body == "" {
		return SendResult{}, errors.New("body is required")
	}
	if req.LocalRequestID == "" {
		req.LocalRequestID = uuid.NewString()
	}

	source, err := s.sourceForAccount(ctx, req.Account)
	if err != nil {
		return SendResult{}, err
	}
	conversationID, err := s.ensureConversation(source.ID, req.ChatID)
	if err != nil {
		return SendResult{}, fmt.Errorf("ensure conversation: %w", err)
	}

	outboxID, err := s.store.InsertWhatsAppOutbox(ctx, store.WhatsAppOutboxInsert{
		LocalRequestID: req.LocalRequestID,
		SourceID:       source.ID,
		ConversationID: sql.NullInt64{Int64: conversationID, Valid: true},
		Kind:           store.WhatsAppOutboxMessage,
		ChatJID:        req.ChatID,
		Body:           sql.NullString{String: req.Body, Valid: true},
	})
	if err != nil {
		return SendResult{}, err
	}
	result := SendResult{
		LocalRequestID: req.LocalRequestID,
		OutboxID:       outboxID,
		ChatJID:        req.ChatID,
		Status:         store.WhatsAppOutboxPending,
	}

	if err := s.store.MarkWhatsAppOutboxSending(ctx, outboxID); err != nil {
		return result, err
	}
	result.Status = store.WhatsAppOutboxSending

	remote, err := s.transport.SendMessage(ctx, TransportSendMessageRequest{
		Account: source.Identifier,
		ChatID:  req.ChatID,
		Body:    req.Body,
	})
	if err != nil {
		_ = s.store.MarkWhatsAppOutboxFailed(ctx, outboxID, err)
		result.Status = store.WhatsAppOutboxFailed
		return result, err
	}
	if remote.ChatJID == "" {
		remote.ChatJID = req.ChatID
	}
	if remote.Timestamp.IsZero() {
		remote.Timestamp = s.now()
	}

	raw, _ := json.Marshal(map[string]any{
		"direction":         "outbound",
		"local_request_id":  req.LocalRequestID,
		"chat_jid":          remote.ChatJID,
		"remote_message_id": remote.RemoteMessageID,
		"body":              req.Body,
		"sent_at":           remote.Timestamp,
	})
	messageID, archiveErr := s.ArchiveInbound(ctx, InboundMessage{
		Account:   source.Identifier,
		ChatJID:   remote.ChatJID,
		SenderJID: source.Identifier,
		MessageID: remote.RemoteMessageID,
		Text:      req.Body,
		Timestamp: remote.Timestamp,
		IsFromMe:  true,
		IsGroup:   isGroupJID(remote.ChatJID),
		RawJSON:   raw,
	})
	if archiveErr != nil {
		_ = s.store.MarkWhatsAppOutboxFailed(ctx, outboxID, archiveErr)
		result.Status = store.WhatsAppOutboxFailed
		return result, archiveErr
	}

	if err := s.store.MarkWhatsAppOutboxSent(ctx, outboxID, remote.RemoteMessageID, messageID); err != nil {
		return result, err
	}
	result.Status = store.WhatsAppOutboxSent
	result.MessageID = messageID
	result.RemoteMessageID = remote.RemoteMessageID
	result.ChatJID = remote.ChatJID
	return result, nil
}

func (s *Service) SendReaction(ctx context.Context, req SendReactionRequest) (SendResult, error) {
	if req.MessageID == 0 {
		return SendResult{}, errors.New("message_id is required")
	}
	if req.LocalRequestID == "" {
		req.LocalRequestID = uuid.NewString()
	}

	ref, err := s.store.GetWhatsAppMessageRef(ctx, req.MessageID)
	if err != nil {
		return SendResult{}, fmt.Errorf("get whatsapp message: %w", err)
	}
	source, err := s.store.GetSourceByID(ref.SourceID)
	if err != nil {
		return SendResult{}, err
	}
	if source.SourceType != store.WhatsAppSourceType {
		return SendResult{}, fmt.Errorf("message source is %q, not whatsapp", source.SourceType)
	}
	if req.Account != "" && req.Account != source.Identifier {
		return SendResult{}, fmt.Errorf("message belongs to whatsapp account %q, not %q", source.Identifier, req.Account)
	}

	outboxID, err := s.store.InsertWhatsAppOutbox(ctx, store.WhatsAppOutboxInsert{
		LocalRequestID:        req.LocalRequestID,
		SourceID:              source.ID,
		ConversationID:        sql.NullInt64{Int64: ref.ConversationID, Valid: true},
		MessageID:             sql.NullInt64{Int64: ref.ID, Valid: true},
		Kind:                  store.WhatsAppOutboxReaction,
		ChatJID:               ref.ChatJID,
		TargetSourceMessageID: sql.NullString{String: ref.SourceMessageID, Valid: true},
		Emoji:                 sql.NullString{String: req.Emoji, Valid: true},
	})
	if err != nil {
		return SendResult{}, err
	}
	result := SendResult{
		LocalRequestID: req.LocalRequestID,
		OutboxID:       outboxID,
		MessageID:      ref.ID,
		ChatJID:        ref.ChatJID,
		Status:         store.WhatsAppOutboxPending,
	}

	if err := s.store.MarkWhatsAppOutboxSending(ctx, outboxID); err != nil {
		return result, err
	}
	result.Status = store.WhatsAppOutboxSending

	senderJID := ref.SenderJID
	if senderJID == "" && ref.IsFromMe {
		senderJID = source.Identifier
	}
	remote, err := s.transport.SendReaction(ctx, TransportSendReactionRequest{
		Account:         source.Identifier,
		ChatJID:         ref.ChatJID,
		SenderJID:       senderJID,
		RemoteMessageID: ref.RemoteMessageID,
		Emoji:           req.Emoji,
	})
	if err != nil {
		_ = s.store.MarkWhatsAppOutboxFailed(ctx, outboxID, err)
		result.Status = store.WhatsAppOutboxFailed
		return result, err
	}
	if remote.RemoteMessageID == "" {
		remote.RemoteMessageID = ref.RemoteMessageID
	}

	reactorID, err := s.store.EnsureParticipantByIdentifier(store.WhatsAppSourceType, source.Identifier, "")
	if err != nil {
		_ = s.store.MarkWhatsAppOutboxFailed(ctx, outboxID, err)
		result.Status = store.WhatsAppOutboxFailed
		return result, err
	}
	if err := s.store.SetReaction(ref.ID, reactorID, reactionTypeEmoji, req.Emoji, s.now()); err != nil {
		_ = s.store.MarkWhatsAppOutboxFailed(ctx, outboxID, err)
		result.Status = store.WhatsAppOutboxFailed
		return result, err
	}
	if err := s.store.MarkWhatsAppOutboxSent(ctx, outboxID, remote.RemoteMessageID, 0); err != nil {
		return result, err
	}
	result.Status = store.WhatsAppOutboxSent
	result.RemoteMessageID = remote.RemoteMessageID
	return result, nil
}

func (s *Service) ArchiveInbound(ctx context.Context, msg InboundMessage) (int64, error) {
	if msg.ChatJID == "" {
		return 0, errors.New("chat_jid is required")
	}
	source, err := s.sourceForAccount(ctx, msg.Account)
	if err != nil {
		return 0, err
	}

	if msg.Reaction != nil {
		return s.archiveReaction(ctx, source.ID, msg)
	}

	if msg.MessageID == "" {
		return 0, errors.New("message_id is required")
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = s.now()
	}
	conversationID, err := s.ensureConversation(source.ID, msg.ChatJID)
	if err != nil {
		return 0, fmt.Errorf("ensure conversation: %w", err)
	}

	senderJID := msg.SenderJID
	if senderJID == "" && msg.IsFromMe {
		senderJID = source.Identifier
	}
	var senderID sql.NullInt64
	if senderJID != "" {
		pid, err := s.store.EnsureParticipantByIdentifier(store.WhatsAppSourceType, senderJID, msg.PushName)
		if err != nil {
			return 0, fmt.Errorf("ensure sender: %w", err)
		}
		senderID = sql.NullInt64{Int64: pid, Valid: true}
		_ = s.store.EnsureConversationParticipant(conversationID, pid, "member")
	}

	body := sql.NullString{String: msg.Text, Valid: msg.Text != ""}
	messageID, err := s.store.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: store.WhatsAppSourceMessageID(msg.ChatJID, msg.MessageID),
		MessageType:     store.WhatsAppMessageType,
		SentAt:          sql.NullTime{Time: msg.Timestamp, Valid: true},
		ReceivedAt:      sql.NullTime{Time: msg.Timestamp, Valid: true},
		InternalDate:    sql.NullTime{Time: msg.Timestamp, Valid: true},
		SenderID:        senderID,
		IsFromMe:        msg.IsFromMe,
		Snippet:         sql.NullString{String: snippet(msg.Text), Valid: msg.Text != ""},
		SizeEstimate:    int64(len(msg.Text)),
	})
	if err != nil {
		return 0, fmt.Errorf("upsert message: %w", err)
	}

	if body.Valid {
		if err := s.store.UpsertMessageBody(messageID, body, sql.NullString{}); err != nil {
			return 0, fmt.Errorf("store body: %w", err)
		}
		_ = s.store.UpsertFTS(messageID, "", body.String, senderJID, "", "")
	}
	raw := msg.RawJSON
	if len(raw) == 0 {
		raw, _ = json.Marshal(msg)
	}
	if len(raw) > 0 {
		if err := s.store.UpsertMessageRawWithFormat(messageID, raw, store.WhatsAppRawFormat); err != nil {
			return 0, fmt.Errorf("store raw: %w", err)
		}
	}

	_ = s.store.RecomputeConversationStats(source.ID)
	return messageID, nil
}

func (s *Service) archiveReaction(ctx context.Context, sourceID int64, msg InboundMessage) (int64, error) {
	targetChatJID := msg.Reaction.TargetChatJID
	if targetChatJID == "" {
		targetChatJID = msg.ChatJID
	}
	if targetChatJID == "" || msg.Reaction.TargetMessageID == "" {
		return 0, errors.New("reaction target chat_jid and message_id are required")
	}
	targetSourceMessageID := store.WhatsAppSourceMessageID(targetChatJID, msg.Reaction.TargetMessageID)
	targetID, err := s.store.GetWhatsAppMessageIDBySource(ctx, sourceID, targetSourceMessageID)
	if err != nil {
		return 0, err
	}
	if targetID == 0 {
		return 0, nil
	}

	reactorJID := msg.SenderJID
	if reactorJID == "" && msg.IsFromMe {
		source, err := s.store.GetSourceByID(sourceID)
		if err == nil {
			reactorJID = source.Identifier
		}
	}
	if reactorJID == "" {
		return 0, errors.New("reaction sender_jid is required")
	}
	reactorID, err := s.store.EnsureParticipantByIdentifier(store.WhatsAppSourceType, reactorJID, msg.PushName)
	if err != nil {
		return 0, fmt.Errorf("ensure reactor: %w", err)
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = s.now()
	}
	if err := s.store.SetReaction(targetID, reactorID, reactionTypeEmoji, msg.Reaction.Emoji, msg.Timestamp); err != nil {
		return 0, err
	}
	return targetID, nil
}

func (s *Service) sourceForAccount(ctx context.Context, account string) (*store.Source, error) {
	account = strings.TrimSpace(account)
	if account == "" {
		account = s.account
	}
	if account == "" {
		status, err := s.transport.Status(ctx)
		if err != nil {
			return nil, fmt.Errorf("whatsapp status: %w", err)
		}
		account = status.AccountJID
		if account == "" {
			account = status.Account
		}
	}
	if account == "" {
		return nil, errors.New("whatsapp account is not paired; run whatsapp-link first")
	}
	source, err := s.store.GetOrCreateSource(store.WhatsAppSourceType, account)
	if err != nil {
		return nil, fmt.Errorf("get whatsapp source: %w", err)
	}
	return source, nil
}

func (s *Service) ensureConversation(sourceID int64, chatJID string) (int64, error) {
	conversationType := "direct_chat"
	if isGroupJID(chatJID) {
		conversationType = "group_chat"
	}
	return s.store.EnsureConversationWithType(sourceID, chatJID, conversationType, "")
}

func isGroupJID(jid string) bool {
	return strings.HasSuffix(jid, "@g.us")
}

func snippet(body string) string {
	body = strings.TrimSpace(body)
	if utf8.RuneCountInString(body) <= 240 {
		return body
	}
	runes := []rune(body)
	return string(runes[:240])
}

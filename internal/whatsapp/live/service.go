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
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const reactionTypeEmoji = "emoji"

type Service struct {
	store          *store.Store
	transport      Transport
	account        string
	loginContext   context.Context
	now            func() time.Time
	notify         func(context.Context, InboundEvent)
	attachmentsDir string
}

type ServiceOptions struct {
	Store        *store.Store
	Transport    Transport
	Account      string
	LoginContext context.Context
	Now          func() time.Time
	// Notify, when set, is called after a message (inbound or outbound
	// echo) has been archived. Reactions do not notify.
	Notify func(context.Context, InboundEvent)
	// AttachmentsDir is the content-addressed attachment store root (same
	// directory Gmail/IMAP sync writes into). When empty, inbound media bytes
	// are not persisted even if they were successfully downloaded.
	AttachmentsDir string
}

type QRPairingTransport interface {
	StartQRPairing(ctx context.Context) error
	PairingState(ctx context.Context) (QRPairingState, error)
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
	loginContext := opts.LoginContext
	if loginContext == nil {
		loginContext = context.Background()
	}
	return &Service{
		store:          opts.Store,
		transport:      opts.Transport,
		account:        strings.TrimSpace(opts.Account),
		loginContext:   loginContext,
		now:            now,
		notify:         opts.Notify,
		attachmentsDir: opts.AttachmentsDir,
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
	status.ApplyDerived()
	return status, nil
}

func (s *Service) Connect(ctx context.Context) error {
	return s.transport.Connect(ctx)
}

func (s *Service) Close() error {
	return s.transport.Close()
}

func (s *Service) Logout(ctx context.Context, req LogoutRequest) (LogoutResult, error) {
	before, err := s.Status(ctx)
	if err != nil {
		return LogoutResult{}, err
	}
	transportResult, err := s.transport.Logout(ctx, TransportLogoutRequest{
		ForceLocal: req.ForceLocal,
	})
	after, statusErr := s.Status(ctx)
	result := LogoutResult{
		StatusBefore:        before,
		StatusAfter:         after,
		RemoteLogout:        transportResult.RemoteLogout,
		LocalSessionCleared: transportResult.LocalSessionCleared,
		ForcedLocalClear:    transportResult.ForcedLocalClear,
	}
	if err != nil {
		return result, err
	}
	if statusErr != nil {
		return result, statusErr
	}
	return result, nil
}

func (s *Service) StartLogin(ctx context.Context) (LoginState, error) {
	status, err := s.Status(ctx)
	if err != nil {
		return LoginState{}, err
	}
	if status.Paired {
		if !status.IsReady() {
			if !status.Connected {
				if err := s.Connect(s.loginContext); err != nil {
					state, stateErr := s.LoginState(ctx)
					if stateErr != nil {
						return LoginState{Status: status}, err
					}
					return state, err
				}
			}
			return s.waitForReady(ctx, status, 15*time.Second)
		}
		return s.LoginState(ctx)
	}

	pairer, ok := s.transport.(QRPairingTransport)
	if !ok {
		return LoginState{Status: status}, errors.New("whatsapp QR login is not supported by this transport")
	}
	if err := pairer.StartQRPairing(s.loginContext); err != nil {
		return LoginState{Status: status}, err
	}
	return s.LoginState(ctx)
}

func (s *Service) waitForReady(ctx context.Context, fallback Status, wait time.Duration) (LoginState, error) {
	if wait <= 0 || fallback.IsReady() {
		return s.LoginState(ctx)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return LoginState{Status: fallback}, ctx.Err()
		case <-deadline.C:
			return s.LoginState(ctx)
		case <-ticker.C:
			state, err := s.LoginState(ctx)
			if err != nil {
				return LoginState{Status: fallback}, err
			}
			if state.Status.IsReady() || !state.Status.Paired || state.Pairing.Error != "" {
				return state, nil
			}
		}
	}
}

func (s *Service) LoginState(ctx context.Context) (LoginState, error) {
	status, err := s.Status(ctx)
	if err != nil {
		return LoginState{}, err
	}
	pairer, ok := s.transport.(QRPairingTransport)
	if !ok {
		return LoginState{Status: status}, nil
	}
	pairing, err := pairer.PairingState(ctx)
	if err != nil {
		return LoginState{Status: status}, err
	}
	return LoginState{
		Status:  status,
		Pairing: pairing,
	}, nil
}

func (s *Service) requireReady(ctx context.Context) (Status, error) {
	status, err := s.Status(ctx)
	if err != nil {
		return Status{}, err
	}
	if !status.IsReady() {
		return status, fmt.Errorf(
			"whatsapp is not ready: paired=%t connected=%t logged_in=%t; run whatsapp_start_login and wait for ready=true before sending",
			status.Paired,
			status.Connected,
			status.LoggedIn,
		)
	}
	return status, nil
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
	if _, err := s.requireReady(ctx); err != nil {
		return SendResult{}, err
	}

	source, err := s.sourceForAccount(ctx, req.Account)
	if err != nil {
		return SendResult{}, err
	}
	conversationID, err := s.ensureConversation(source.ID, req.ChatID)
	if err != nil {
		return SendResult{}, fmt.Errorf("ensure conversation: %w", err)
	}

	outboxID, created, err := s.store.InsertWhatsAppOutboxIfAbsent(ctx, store.WhatsAppOutboxInsert{
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
	if !created {
		return s.existingSendResult(ctx, outboxID, store.WhatsAppOutboxMessage, req.ChatID, req.Body)
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
		Account:  source.Identifier,
		ChatID:   req.ChatID,
		Body:     req.Body,
		Mentions: req.Mentions,
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
	result.RemoteMessageID = remote.RemoteMessageID
	result.MessageID = messageID
	return result, nil
}

func (s *Service) existingSendResult(
	ctx context.Context,
	outboxID int64,
	kind string,
	chatJID string,
	payload string,
) (SendResult, error) {
	record, err := s.store.GetWhatsAppOutbox(ctx, outboxID)
	if err != nil {
		return SendResult{}, err
	}
	existingPayload := record.Body.String
	if kind == store.WhatsAppOutboxReaction {
		existingPayload = record.Emoji.String
	}
	if record.Kind != kind || record.ChatJID != chatJID || existingPayload != payload {
		return SendResult{}, fmt.Errorf("local_request_id %q was already used for a different whatsapp operation", record.LocalRequestID)
	}
	result := SendResult{
		LocalRequestID:  record.LocalRequestID,
		OutboxID:        record.ID,
		MessageID:       record.MessageID.Int64,
		RemoteMessageID: record.RemoteMessageID.String,
		ChatJID:         record.ChatJID,
		Status:          record.Status,
	}
	if record.Status == store.WhatsAppOutboxFailed {
		message := record.ErrorText.String
		if message == "" {
			message = "whatsapp send failed"
		}
		return result, errors.New(message)
	}
	return result, nil
}

func (s *Service) SendReaction(ctx context.Context, req SendReactionRequest) (SendResult, error) {
	if req.MessageID == 0 {
		return SendResult{}, errors.New("message_id is required")
	}
	req.Emoji = strings.TrimSpace(req.Emoji)
	if req.LocalRequestID == "" {
		req.LocalRequestID = uuid.NewString()
	}
	if _, err := s.requireReady(ctx); err != nil {
		return SendResult{}, err
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

	outboxID, created, err := s.store.InsertWhatsAppOutboxIfAbsent(ctx, store.WhatsAppOutboxInsert{
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
	if !created {
		return s.existingSendResult(ctx, outboxID, store.WhatsAppOutboxReaction, ref.ChatJID, req.Emoji)
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
	return s.archiveInbound(ctx, msg, true, true)
}

// ArchiveHistorySync stores a WhatsApp history-sync batch without emitting
// live-message webhooks. Conversation stats are recomputed once per source
// after the batch instead of once per message.
func (s *Service) ArchiveHistorySync(ctx context.Context, messages []InboundMessage) error {
	accounts := make(map[string]struct{})
	failures := 0
	var firstErr error
	for _, msg := range messages {
		accounts[msg.Account] = struct{}{}
		if _, err := s.archiveInbound(ctx, msg, false, false); err != nil {
			failures++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	for account := range accounts {
		source, err := s.sourceForAccount(ctx, account)
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.store.RecomputeConversationStats(source.ID); err != nil {
			failures++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("archive history sync: %d failures (first: %w)", failures, firstErr)
	}
	return nil
}

func (s *Service) archiveInbound(ctx context.Context, msg InboundMessage, recomputeStats, notify bool) (int64, error) {
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
	conversationID, err := s.ensureConversationWithTitle(source.ID, msg.ChatJID, msg.ChatTitle)
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

	if msg.Attachment != nil {
		if err := s.storeInboundAttachment(messageID, msg.Attachment); err != nil {
			return 0, fmt.Errorf("store attachment: %w", err)
		}
	}

	if recomputeStats {
		_ = s.store.RecomputeConversationStats(source.ID)
	}

	if notify && s.notify != nil {
		s.notify(ctx, InboundEvent{
			Account:         source.Identifier,
			Source:          store.WhatsAppSourceType,
			ChatJID:         msg.ChatJID,
			SenderJID:       senderJID,
			PushName:        msg.PushName,
			MessageID:       msg.MessageID,
			SourceMessageID: store.WhatsAppSourceMessageID(msg.ChatJID, msg.MessageID),
			StoreMessageID:  messageID,
			Body:            msg.Text,
			Timestamp:       msg.Timestamp,
			IsFromMe:        msg.IsFromMe,
			IsGroup:         msg.IsGroup,
		})
	}
	return messageID, nil
}

// storeInboundAttachment persists a downloaded WhatsApp media payload through
// the same content-addressed attachment store Gmail/IMAP sync writes into
// (see internal/export.StoreAttachmentFile and internal/sync.Syncer.storeAttachment).
//
// att.Data is empty when the download failed (expired media key, network
// error, ...); that failure was already logged where the download was
// attempted (WhatsmeowTransport.downloadMediaAttachment), so this is a no-op
// beyond keeping the message's attachment stats consistent with the real
// attachments table — no attachments row is created for bytes that were
// never fetched, matching the convention the offline WhatsApp DYI importer
// uses for the same "no content" case.
func (s *Service) storeInboundAttachment(messageID int64, att *InboundAttachment) error {
	if att == nil {
		return nil
	}
	var storeErr error
	if len(att.Data) > 0 && s.attachmentsDir != "" {
		mimeAtt := &mime.Attachment{
			Filename:    att.Filename,
			ContentType: att.MimeType,
			Content:     att.Data,
		}
		storagePath, err := export.StoreAttachmentFile(s.attachmentsDir, mimeAtt)
		if err != nil {
			storeErr = fmt.Errorf("store whatsapp attachment file: %w", err)
		} else if storagePath != "" {
			if err := s.store.UpsertAttachment(
				messageID, att.Filename, att.MimeType, storagePath, mimeAtt.ContentHash, len(att.Data),
			); err != nil {
				storeErr = fmt.Errorf("upsert whatsapp attachment: %w", err)
			}
		}
	}
	if err := s.store.RecomputeMessageAttachmentStats(messageID); err != nil && storeErr == nil {
		storeErr = fmt.Errorf("recompute whatsapp attachment stats: %w", err)
	}
	return storeErr
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
	return s.ensureConversationWithTitle(sourceID, chatJID, "")
}

func (s *Service) ensureConversationWithTitle(sourceID int64, chatJID, title string) (int64, error) {
	conversationType := "direct_chat"
	if isGroupJID(chatJID) {
		conversationType = "group_chat"
	}
	return s.store.EnsureConversationWithType(sourceID, chatJID, conversationType, strings.TrimSpace(title))
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

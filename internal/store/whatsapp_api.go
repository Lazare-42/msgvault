package store

import (
	"context"
	"database/sql"
	"fmt"
)

// WhatsAppChatSummary is one WhatsApp conversation for the local live API.
type WhatsAppChatSummary struct {
	ConversationID     int64          `json:"conversation_id"`
	Account            string         `json:"account"`
	ChatJID            string         `json:"chat_jid"`
	Title              sql.NullString `json:"-"`
	ConversationType   string         `json:"conversation_type"`
	MessageCount       int64          `json:"message_count"`
	LastMessageAt      sql.NullTime   `json:"-"`
	LastMessagePreview sql.NullString `json:"-"`
}

// WhatsAppMessageRecord is one archived WhatsApp message for the local
// live API. IDs are ascending, so callers can page with after_id cursors.
type WhatsAppMessageRecord struct {
	ID              int64          `json:"id"`
	ChatJID         string         `json:"chat_jid"`
	SourceMessageID string         `json:"source_message_id"`
	SenderJID       string         `json:"sender_jid"`
	SenderName      sql.NullString `json:"-"`
	IsFromMe        bool           `json:"is_from_me"`
	SentAt          sql.NullTime   `json:"-"`
	Body            sql.NullString `json:"-"`
}

// ListWhatsAppChats returns WhatsApp conversations ordered by most recent
// activity.
func (s *Store) ListWhatsAppChats(ctx context.Context, limit int) ([]WhatsAppChatSummary, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, src.identifier, c.source_conversation_id,
		       c.title, c.conversation_type, c.message_count,
		       c.last_message_at, c.last_message_preview
		FROM conversations c
		JOIN sources src ON src.id = c.source_id
		WHERE src.source_type = ?
		  AND c.source_conversation_id IS NOT NULL
		ORDER BY c.last_message_at DESC
		LIMIT ?
	`, WhatsAppSourceType, limit)
	if err != nil {
		return nil, fmt.Errorf("list whatsapp chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var chats []WhatsAppChatSummary
	for rows.Next() {
		var c WhatsAppChatSummary
		if err := rows.Scan(
			&c.ConversationID, &c.Account, &c.ChatJID,
			&c.Title, &c.ConversationType, &c.MessageCount,
			&c.LastMessageAt, &c.LastMessagePreview,
		); err != nil {
			return nil, fmt.Errorf("scan whatsapp chat: %w", err)
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// ListWhatsAppMessagesAfter returns archived WhatsApp messages for one chat
// with id > afterID, oldest first.
func (s *Store) ListWhatsAppMessagesAfter(ctx context.Context, chatJID string, afterID int64, limit int) ([]WhatsAppMessageRecord, error) {
	if chatJID == "" {
		return nil, fmt.Errorf("chat_jid is required")
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, c.source_conversation_id, m.source_message_id,
		       COALESCE(pi.identifier_value, ''), p.display_name,
		       m.is_from_me, m.sent_at, mb.body_text
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		LEFT JOIN participants p ON p.id = m.sender_id
		LEFT JOIN participant_identifiers pi
		  ON pi.participant_id = p.id AND pi.identifier_type = ?
		WHERE src.source_type = ?
		  AND m.message_type = ?
		  AND c.source_conversation_id = ?
		  AND m.id > ?
		  AND m.deleted_at IS NULL
		ORDER BY m.id ASC
		LIMIT ?
	`, WhatsAppIdentifierType, WhatsAppSourceType, WhatsAppMessageType, chatJID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list whatsapp messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []WhatsAppMessageRecord
	for rows.Next() {
		var m WhatsAppMessageRecord
		if err := rows.Scan(
			&m.ID, &m.ChatJID, &m.SourceMessageID,
			&m.SenderJID, &m.SenderName,
			&m.IsFromMe, &m.SentAt, &m.Body,
		); err != nil {
			return nil, fmt.Errorf("scan whatsapp message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

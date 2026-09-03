package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
// live API. IDs are ascending in archive order, so callers can page with
// after_id cursors; after a history backfill older messages may carry
// higher IDs than newer ones.
type WhatsAppMessageRecord struct {
	ID              int64          `json:"id"`
	Account         string         `json:"account"`
	ChatJID         string         `json:"chat_jid"`
	SourceMessageID string         `json:"source_message_id"`
	SenderJID       string         `json:"sender_jid"`
	SenderName      sql.NullString `json:"-"`
	IsFromMe        bool           `json:"is_from_me"`
	SentAt          sql.NullTime   `json:"-"`
	Body            sql.NullString `json:"-"`
}

// MaxWhatsAppChatPage caps one ListWhatsAppChats page.
const MaxWhatsAppChatPage = 1000

// MaxWhatsAppMessagePage caps one ListWhatsAppMessagesAfter page.
const MaxWhatsAppMessagePage = 500

// WhatsAppChatFilter narrows ListWhatsAppChats.
type WhatsAppChatFilter struct {
	// Account is an exact WhatsApp source identifier; empty matches every
	// archived WhatsApp account.
	Account string
	// Query is a case-insensitive substring matched against the chat title
	// and chat JID; empty matches every chat.
	Query string
	// Limit caps the page at 1..MaxWhatsAppChatPage rows; other values read
	// as MaxWhatsAppChatPage.
	Limit int
	// Offset skips rows of the most-recent-activity-first ordering.
	Offset int
}

// WhatsAppMessageFilter narrows ListWhatsAppMessagesAfter.
type WhatsAppMessageFilter struct {
	// Account is an exact WhatsApp source identifier; empty matches every
	// archived WhatsApp account, which can mix accounts that share a chat JID.
	Account string
	// ChatJID is required.
	ChatJID string
	// AfterID returns only messages with a greater archive ID.
	AfterID int64
	// Limit caps the page at 1..MaxWhatsAppMessagePage rows; other values
	// read as MaxWhatsAppMessagePage.
	Limit int
}

// ListWhatsAppChats returns WhatsApp conversations ordered by most recent
// activity.
func (s *Store) ListWhatsAppChats(ctx context.Context, filter WhatsAppChatFilter) ([]WhatsAppChatSummary, error) {
	limit := filter.Limit
	if limit <= 0 || limit > MaxWhatsAppChatPage {
		limit = MaxWhatsAppChatPage
	}
	query := `
		SELECT c.id, src.identifier, c.source_conversation_id,
		       c.title, c.conversation_type, c.message_count,
		       c.last_message_at, c.last_message_preview
		FROM conversations c
		JOIN sources src ON src.id = c.source_id
		WHERE src.source_type = ?
		  AND c.source_conversation_id IS NOT NULL`
	args := []any{WhatsAppSourceType}
	if filter.Account != "" {
		query += ` AND src.identifier = ?`
		args = append(args, filter.Account)
	}
	if term := strings.TrimSpace(filter.Query); term != "" {
		pattern := "%" + escapeLike(strings.ToLower(term)) + "%"
		query += ` AND (LOWER(COALESCE(c.title, '')) LIKE ? ESCAPE '\' OR LOWER(c.source_conversation_id) LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern)
	}
	query += ` ORDER BY c.last_message_at DESC, c.id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, max(filter.Offset, 0))

	rows, err := s.db.QueryContext(ctx, query, args...)
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
// with id > AfterID, in ascending archive ID order.
func (s *Store) ListWhatsAppMessagesAfter(ctx context.Context, filter WhatsAppMessageFilter) ([]WhatsAppMessageRecord, error) {
	if filter.ChatJID == "" {
		return nil, fmt.Errorf("chat_jid is required")
	}
	limit := filter.Limit
	if limit <= 0 || limit > MaxWhatsAppMessagePage {
		limit = MaxWhatsAppMessagePage
	}
	query := `
		SELECT m.id, src.identifier, c.source_conversation_id, m.source_message_id,
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
		  AND m.deleted_at IS NULL`
	args := []any{WhatsAppIdentifierType, WhatsAppSourceType, WhatsAppMessageType, filter.ChatJID, filter.AfterID}
	if filter.Account != "" {
		query += ` AND src.identifier = ?`
		args = append(args, filter.Account)
	}
	query += ` ORDER BY m.id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list whatsapp messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []WhatsAppMessageRecord
	for rows.Next() {
		var m WhatsAppMessageRecord
		if err := rows.Scan(
			&m.ID, &m.Account, &m.ChatJID, &m.SourceMessageID,
			&m.SenderJID, &m.SenderName,
			&m.IsFromMe, &m.SentAt, &m.Body,
		); err != nil {
			return nil, fmt.Errorf("scan whatsapp message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

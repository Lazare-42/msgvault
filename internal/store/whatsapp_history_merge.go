package store

import (
	"database/sql"
	"fmt"
	"time"
)

// This file provides read-only helpers consumed by the merge-whatsapp-history
// maintenance command (internal/whatsapp/historymerge), which backfills
// WhatsApp-sourced history from one archive home into another after they were
// split into separate MSGVAULT_HOME directories. Every method here is a plain
// SELECT so it works against a Store opened via OpenReadOnly.

// WhatsAppMergeConversation is one WhatsApp conversation row read from a
// source archive for the purpose of replaying it into a target archive.
type WhatsAppMergeConversation struct {
	ID                   int64
	SourceConversationID string
	ConversationType     string
	Title                string
}

// ListWhatsAppConversationsForSource returns every WhatsApp conversation
// belonging to sourceID, ordered by id for deterministic replay.
func (s *Store) ListWhatsAppConversationsForSource(sourceID int64) ([]WhatsAppMergeConversation, error) {
	rows, err := s.db.Query(`
		SELECT id, COALESCE(source_conversation_id, ''), conversation_type, COALESCE(title, '')
		FROM conversations
		WHERE source_id = ?
		  AND source_conversation_id IS NOT NULL
		  AND source_conversation_id != ''
		ORDER BY id
	`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list whatsapp conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WhatsAppMergeConversation
	for rows.Next() {
		var c WhatsAppMergeConversation
		if err := rows.Scan(&c.ID, &c.SourceConversationID, &c.ConversationType, &c.Title); err != nil {
			return nil, fmt.Errorf("scan whatsapp conversation: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WhatsAppMergeMessage is one WhatsApp message row read from a source
// archive for the purpose of replaying it into a target archive. Only the
// fields the merge engine needs to reconstruct an equivalent Message are
// included; message_bodies and message_raw are fetched separately by PK
// lookup (see GetMessageBodyText / Store.GetMessageRaw) to keep this bulk
// list query off those tables per the project's SQL guidelines.
type WhatsAppMergeMessage struct {
	ID              int64
	SourceMessageID string
	SentAt          sql.NullTime
	ReceivedAt      sql.NullTime
	InternalDate    sql.NullTime
	SenderID        sql.NullInt64
	IsFromMe        bool
	IdentityIsFromMe bool
	Snippet         sql.NullString
	SizeEstimate    int64
}

// ListWhatsAppMessagesForConversation returns every live (non-deleted)
// WhatsApp message in conversationID, ordered by id for deterministic
// replay and stable reply-chain resolution.
func (s *Store) ListWhatsAppMessagesForConversation(conversationID int64) ([]WhatsAppMergeMessage, error) {
	rows, err := s.db.Query(`
		SELECT id, source_message_id, sent_at, received_at, internal_date,
		       sender_id, is_from_me, identity_is_from_me, snippet, size_estimate
		FROM messages
		WHERE conversation_id = ?
		  AND message_type = ?
		  AND deleted_at IS NULL
		ORDER BY id
	`, conversationID, WhatsAppMessageType)
	if err != nil {
		return nil, fmt.Errorf("list whatsapp messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WhatsAppMergeMessage
	for rows.Next() {
		var m WhatsAppMergeMessage
		if err := rows.Scan(
			&m.ID, &m.SourceMessageID, &m.SentAt, &m.ReceivedAt, &m.InternalDate,
			&m.SenderID, &m.IsFromMe, &m.IdentityIsFromMe, &m.Snippet, &m.SizeEstimate,
		); err != nil {
			return nil, fmt.Errorf("scan whatsapp message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMessageBodyText fetches body_text/body_html for messageID by primary
// key. found is false when the message has no message_bodies row (e.g. a
// media-only message with no caption).
func (s *Store) GetMessageBodyText(messageID int64) (bodyText, bodyHTML sql.NullString, found bool, err error) {
	err = s.db.QueryRow(`
		SELECT body_text, body_html FROM message_bodies WHERE message_id = ?
	`, messageID).Scan(&bodyText, &bodyHTML)
	if err == nil {
		return bodyText, bodyHTML, true, nil
	}
	if err == sql.ErrNoRows {
		return sql.NullString{}, sql.NullString{}, false, nil
	}
	return sql.NullString{}, sql.NullString{}, false, fmt.Errorf("get message body: %w", err)
}

// GetParticipantIdentifier resolves the identifierType identifier (e.g.
// "whatsapp") for participantID, preferring the primary identifier row.
// found is false when the participant has no identifier of that type.
func (s *Store) GetParticipantIdentifier(participantID int64, identifierType string) (value, displayName string, found bool, err error) {
	var display sql.NullString
	err = s.db.QueryRow(`
		SELECT pi.identifier_value, p.display_name
		FROM participant_identifiers pi
		JOIN participants p ON p.id = pi.participant_id
		WHERE pi.participant_id = ? AND pi.identifier_type = ?
		ORDER BY pi.is_primary DESC, pi.id ASC
		LIMIT 1
	`, participantID, identifierType).Scan(&value, &display)
	if err == nil {
		return value, display.String, true, nil
	}
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return "", "", false, fmt.Errorf("get participant identifier: %w", err)
}

// WhatsAppMergeReaction is one active (non-removed) reaction on a WhatsApp
// message, read from a source archive for replay.
type WhatsAppMergeReaction struct {
	ParticipantID int64
	ReactionType  string
	ReactionValue string
	CreatedAt     time.Time
}

// ListActiveWhatsAppReactions returns every reaction on messageID that has
// not been retracted (removed_at IS NULL). Retracted reactions are not
// replayed — they would misrepresent the current state as active.
func (s *Store) ListActiveWhatsAppReactions(messageID int64) ([]WhatsAppMergeReaction, error) {
	rows, err := s.db.Query(`
		SELECT participant_id, reaction_type, reaction_value, created_at
		FROM reactions
		WHERE message_id = ? AND removed_at IS NULL
	`, messageID)
	if err != nil {
		return nil, fmt.Errorf("list active reactions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WhatsAppMergeReaction
	for rows.Next() {
		var r WhatsAppMergeReaction
		if err := rows.Scan(&r.ParticipantID, &r.ReactionType, &r.ReactionValue, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan active reaction: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

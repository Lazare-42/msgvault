package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	WhatsAppSourceType     = "whatsapp"
	WhatsAppMessageType    = "whatsapp"
	WhatsAppIdentifierType = "whatsapp"
	WhatsAppRawFormat      = "whatsapp_json"
	WhatsAppOutboxMessage  = "message"
	WhatsAppOutboxReaction = "reaction"
	WhatsAppOutboxPending  = "pending"
	WhatsAppOutboxSending  = "sending"
	WhatsAppOutboxSent     = "sent"
	WhatsAppOutboxFailed   = "failed"
)

type WhatsAppOutboxInsert struct {
	LocalRequestID        string
	SourceID              int64
	ConversationID        sql.NullInt64
	MessageID             sql.NullInt64
	Kind                  string
	ChatJID               string
	TargetSourceMessageID sql.NullString
	Body                  sql.NullString
	Emoji                 sql.NullString
}

type WhatsAppOutboxRecord struct {
	ID                    int64
	LocalRequestID        string
	SourceID              int64
	ConversationID        sql.NullInt64
	MessageID             sql.NullInt64
	Kind                  string
	ChatJID               string
	TargetSourceMessageID sql.NullString
	Body                  sql.NullString
	Emoji                 sql.NullString
	Status                string
	RemoteMessageID       sql.NullString
	ErrorText             sql.NullString
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type WhatsAppMessageRef struct {
	ID              int64
	SourceID        int64
	ConversationID  int64
	SourceMessageID string
	ChatJID         string
	RemoteMessageID string
	SenderJID       string
	IsFromMe        bool
}

func WhatsAppSourceMessageID(chatJID, messageID string) string {
	return chatJID + "/" + messageID
}

func SplitWhatsAppSourceMessageID(sourceMessageID string) (chatJID, messageID string, ok bool) {
	chatJID, messageID, ok = strings.Cut(sourceMessageID, "/")
	return chatJID, messageID, ok && chatJID != "" && messageID != ""
}

func (s *Store) InsertWhatsAppOutbox(ctx context.Context, in WhatsAppOutboxInsert) (int64, error) {
	id, _, err := s.InsertWhatsAppOutboxIfAbsent(ctx, in)
	return id, err
}

// InsertWhatsAppOutboxIfAbsent reserves a local request id exactly once.
// created=false means caller must return existing outbox state, never resend.
func (s *Store) InsertWhatsAppOutboxIfAbsent(ctx context.Context, in WhatsAppOutboxInsert) (id int64, created bool, err error) {
	if in.LocalRequestID == "" {
		return 0, false, errors.New("local_request_id is required")
	}
	if in.SourceID == 0 {
		return 0, false, errors.New("source_id is required")
	}
	if in.Kind != WhatsAppOutboxMessage && in.Kind != WhatsAppOutboxReaction {
		return 0, false, fmt.Errorf("invalid outbox kind %q", in.Kind)
	}
	if in.ChatJID == "" {
		return 0, false, errors.New("chat_jid is required")
	}

	err = s.db.QueryRowContext(ctx, fmt.Sprintf(`
		INSERT INTO whatsapp_outbox (
			local_request_id, source_id, conversation_id, message_id,
			kind, chat_jid, target_source_message_id, body, emoji,
			status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, %s, %s)
		ON CONFLICT(local_request_id) DO NOTHING
		RETURNING id
	`, s.dialect.Now(), s.dialect.Now()),
		in.LocalRequestID, in.SourceID, in.ConversationID, in.MessageID,
		in.Kind, in.ChatJID, in.TargetSourceMessageID, in.Body, in.Emoji,
		WhatsAppOutboxPending,
	).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("insert whatsapp outbox: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM whatsapp_outbox WHERE local_request_id = ?`,
		in.LocalRequestID,
	).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("get existing whatsapp outbox: %w", err)
	}
	return id, false, nil
}

func (s *Store) MarkWhatsAppOutboxSending(ctx context.Context, id int64) error {
	return s.updateWhatsAppOutboxStatus(ctx, id, WhatsAppOutboxSending, "", nil, sql.NullInt64{})
}

func (s *Store) MarkWhatsAppOutboxSent(ctx context.Context, id int64, remoteMessageID string, archiveMessageID int64) error {
	return s.updateWhatsAppOutboxStatus(ctx, id, WhatsAppOutboxSent, remoteMessageID, nil, sql.NullInt64{
		Int64: archiveMessageID,
		Valid: archiveMessageID != 0,
	})
}

func (s *Store) MarkWhatsAppOutboxFailed(ctx context.Context, id int64, sendErr error) error {
	text := ""
	if sendErr != nil {
		text = sendErr.Error()
	}
	return s.updateWhatsAppOutboxStatus(ctx, id, WhatsAppOutboxFailed, "", &text, sql.NullInt64{})
}

func (s *Store) updateWhatsAppOutboxStatus(ctx context.Context, id int64, status, remoteMessageID string, errorText *string, archiveMessageID sql.NullInt64) error {
	setMessage := ""
	args := []any{status}
	if remoteMessageID != "" {
		setMessage += ", remote_message_id = ?"
		args = append(args, remoteMessageID)
	}
	if errorText != nil {
		setMessage += ", error_text = ?"
		args = append(args, *errorText)
	} else if status == WhatsAppOutboxSent {
		setMessage += ", error_text = NULL"
	}
	if archiveMessageID.Valid {
		setMessage += ", message_id = ?"
		args = append(args, archiveMessageID.Int64)
	}
	args = append(args, id)

	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE whatsapp_outbox
		SET status = ?%s, updated_at = %s
		WHERE id = ?
	`, setMessage, s.dialect.Now()), args...)
	if err != nil {
		return fmt.Errorf("update whatsapp outbox: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check whatsapp outbox rows: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("whatsapp outbox %d not found", id)
	}
	return nil
}

func (s *Store) GetWhatsAppOutbox(ctx context.Context, id int64) (*WhatsAppOutboxRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, local_request_id, source_id, conversation_id, message_id,
		       kind, chat_jid, target_source_message_id, body, emoji, status,
		       remote_message_id, error_text, created_at, updated_at
		FROM whatsapp_outbox
		WHERE id = ?
	`, id)
	var rec WhatsAppOutboxRecord
	if err := row.Scan(
		&rec.ID, &rec.LocalRequestID, &rec.SourceID, &rec.ConversationID, &rec.MessageID,
		&rec.Kind, &rec.ChatJID, &rec.TargetSourceMessageID, &rec.Body, &rec.Emoji,
		&rec.Status, &rec.RemoteMessageID, &rec.ErrorText, &rec.CreatedAt, &rec.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *Store) GetWhatsAppMessageIDBySource(ctx context.Context, sourceID int64, sourceMessageID string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id
		FROM messages
		WHERE source_id = ? AND source_message_id = ? AND message_type = 'whatsapp'
	`, sourceID, sourceMessageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get whatsapp message: %w", err)
	}
	return id, nil
}

// WhatsAppOldestMessageAnchor is the earliest archived message msgvault has
// for a WhatsApp chat, used as the "last known message" anchor for an
// on-demand history-sync request (see live.Service.RequestHistorySync):
// WhatsApp is asked for messages strictly older than this one.
type WhatsAppOldestMessageAnchor struct {
	SourceMessageID string
	IsFromMe        bool
	SentAt          time.Time
}

// GetOldestWhatsAppMessage returns the earliest archived message (by
// sent_at, then id) for the given WhatsApp chat within sourceID, or nil if no
// message has been archived for that chat yet. A single ORDER BY ... LIMIT 1
// lookup — no message_bodies join (see CLAUDE.md SQL guidelines); only
// source_message_id/is_from_me/sent_at are needed to build a history-sync
// anchor.
func (s *Store) GetOldestWhatsAppMessage(ctx context.Context, sourceID int64, chatJID string) (*WhatsAppOldestMessageAnchor, error) {
	if sourceID == 0 {
		return nil, errors.New("source_id is required")
	}
	if chatJID == "" {
		return nil, errors.New("chat_jid is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT m.source_message_id, m.is_from_me, m.sent_at
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ?
		  AND m.message_type = ?
		  AND c.source_conversation_id = ?
		  AND m.deleted_at IS NULL
		  AND m.sent_at IS NOT NULL
		ORDER BY m.sent_at ASC, m.id ASC
		LIMIT 1
	`, sourceID, WhatsAppMessageType, chatJID)
	var anchor WhatsAppOldestMessageAnchor
	var sentAt sql.NullTime
	if err := row.Scan(&anchor.SourceMessageID, &anchor.IsFromMe, &sentAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get oldest whatsapp message: %w", err)
	}
	anchor.SentAt = sentAt.Time
	return &anchor, nil
}

// SetReaction replaces the active reaction from participantID on messageID.
// Empty reactionValue clears the active reaction without inserting a new one.
func (s *Store) SetReaction(messageID, participantID int64, reactionType, reactionValue string, at time.Time) error {
	if messageID == 0 || participantID == 0 {
		return errors.New("message_id and participant_id are required")
	}
	if reactionType == "" {
		return errors.New("reaction_type is required")
	}
	return s.withTx(func(tx *loggedTx) error {
		if _, err := tx.Exec(`
			UPDATE reactions
			SET removed_at = ?
			WHERE message_id = ?
			  AND participant_id = ?
			  AND reaction_type = ?
			  AND removed_at IS NULL
			  AND reaction_value != ?
		`, at, messageID, participantID, reactionType, reactionValue); err != nil {
			return fmt.Errorf("clear old reactions: %w", err)
		}
		if reactionValue == "" {
			if _, err := tx.Exec(`
				UPDATE reactions
				SET removed_at = ?
				WHERE message_id = ?
				  AND participant_id = ?
				  AND reaction_type = ?
				  AND removed_at IS NULL
			`, at, messageID, participantID, reactionType); err != nil {
				return fmt.Errorf("clear reactions: %w", err)
			}
			return nil
		}
		if _, err := tx.Exec(`
			INSERT INTO reactions (message_id, participant_id, reaction_type, reaction_value, created_at, removed_at)
			VALUES (?, ?, ?, ?, ?, NULL)
			ON CONFLICT(message_id, participant_id, reaction_type, reaction_value) DO UPDATE SET
				created_at = excluded.created_at,
				removed_at = NULL
		`, messageID, participantID, reactionType, reactionValue, at); err != nil {
			return fmt.Errorf("upsert reaction: %w", err)
		}
		return nil
	})
}

func (s *Store) GetWhatsAppMessageRef(ctx context.Context, messageID int64) (*WhatsAppMessageRef, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.source_id, m.conversation_id, m.source_message_id,
		       c.source_conversation_id, COALESCE(pi.identifier_value, ''),
		       m.is_from_me
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN participants p ON p.id = m.sender_id
		LEFT JOIN participant_identifiers pi
		  ON pi.participant_id = p.id AND pi.identifier_type = 'whatsapp'
		WHERE m.id = ? AND m.message_type = 'whatsapp'
		ORDER BY pi.is_primary DESC, pi.id ASC
		LIMIT 1
	`, messageID)
	var ref WhatsAppMessageRef
	if err := row.Scan(
		&ref.ID, &ref.SourceID, &ref.ConversationID, &ref.SourceMessageID,
		&ref.ChatJID, &ref.SenderJID, &ref.IsFromMe,
	); err != nil {
		return nil, err
	}
	chatJID, remoteID, ok := SplitWhatsAppSourceMessageID(ref.SourceMessageID)
	if ok {
		if ref.ChatJID == "" {
			ref.ChatJID = chatJID
		}
		ref.RemoteMessageID = remoteID
	} else {
		ref.RemoteMessageID = ref.SourceMessageID
	}
	return &ref, nil
}

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/sqliteutil"
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

// ErrWhatsAppChatCursorInvalid reports a ListWhatsAppChats cursor that is
// malformed or was issued for a different account, query, or order.
var ErrWhatsAppChatCursorInvalid = errors.New("invalid whatsapp chat cursor")

// ErrWhatsAppChatOrderInvalid reports an unknown WhatsAppChatOrder.
var ErrWhatsAppChatOrderInvalid = errors.New("invalid whatsapp chat order")

// WhatsAppChatOrder selects the ListWhatsAppChats ordering.
type WhatsAppChatOrder string

const (
	// WhatsAppChatOrderRecent lists the most recently active chat first and
	// chats without activity last. Paging over it is best-effort: a chat
	// that gains activity while a caller pages moves above the cursor and
	// is not revisited by that walk; it surfaces at the top of a fresh one.
	WhatsAppChatOrderRecent WhatsAppChatOrder = "recent"
	// WhatsAppChatOrderCreated lists chats by ascending conversation id, an
	// immutable key, so a walk visits every chat that existed when it began
	// and chats created meanwhile appear at its end.
	WhatsAppChatOrderCreated WhatsAppChatOrder = "created"
)

// WhatsAppChatCursor is a keyset position in a chat ordering. Paging by
// position rather than by offset keeps a page from repeating or skipping the
// chats that did not move when the list changes between pages.
type WhatsAppChatCursor struct {
	// ActivityEpoch is the unix-second last-activity key of the chat the
	// page ended on under WhatsAppChatOrderRecent; it is meaningful only when
	// HasActivity is set. Chats without any activity sort after every chat
	// with activity. WhatsAppChatOrderCreated uses ConversationID alone.
	ActivityEpoch  int64
	HasActivity    bool
	ConversationID int64
}

// WhatsAppChatFilter narrows ListWhatsAppChats.
type WhatsAppChatFilter struct {
	// Account is an exact WhatsApp source identifier; empty matches every
	// archived WhatsApp account.
	Account string
	// Query is a case-insensitive (Unicode-aware) substring matched against
	// the chat title and chat JID; empty matches every chat.
	Query string
	// Limit caps the page at 1..MaxWhatsAppChatPage rows; other values read
	// as MaxWhatsAppChatPage.
	Limit int
	// Order defaults to WhatsAppChatOrderRecent when empty.
	Order WhatsAppChatOrder
	// After resumes after the chat a previous page ended on. Nil starts the
	// ordering from its first chat.
	After *WhatsAppChatCursor
}

// WhatsAppChatPage is one ListWhatsAppChats result.
type WhatsAppChatPage struct {
	Chats []WhatsAppChatSummary
	// HasMore reports that older chats follow; Next then resumes after the
	// last chat of this page.
	HasMore bool
	Next    WhatsAppChatCursor
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

const whatsAppChatCursorVersion = 1

// whatsAppChatCursorWire is the opaque cursor's JSON shape.
type whatsAppChatCursorWire struct {
	Version int    `json:"v"`
	Epoch   *int64 `json:"at,omitempty"`
	ID      int64  `json:"id"`
	Filter  string `json:"f"`
}

func whatsAppChatFilterKey(filter WhatsAppChatFilter) string {
	digest := sha256.Sum256([]byte(filter.Account + "\x00" + whatsAppChatQueryTerm(filter) + "\x00" + string(whatsAppChatOrder(filter))))
	return hex.EncodeToString(digest[:8])
}

func whatsAppChatQueryTerm(filter WhatsAppChatFilter) string {
	return strings.ToLower(strings.TrimSpace(filter.Query))
}

func whatsAppChatOrder(filter WhatsAppChatFilter) WhatsAppChatOrder {
	if filter.Order == "" {
		return WhatsAppChatOrderRecent
	}
	return filter.Order
}

// EncodeWhatsAppChatCursor renders a cursor as an opaque token bound to the
// filter's account, query, and order, so a token cannot silently resume a
// different listing.
func EncodeWhatsAppChatCursor(filter WhatsAppChatFilter, cursor WhatsAppChatCursor) string {
	wire := whatsAppChatCursorWire{
		Version: whatsAppChatCursorVersion,
		ID:      cursor.ConversationID,
		Filter:  whatsAppChatFilterKey(filter),
	}
	if cursor.HasActivity {
		epoch := cursor.ActivityEpoch
		wire.Epoch = &epoch
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		panic(err) // fixed shape of scalars cannot fail to marshal
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// DecodeWhatsAppChatCursor parses a token produced by EncodeWhatsAppChatCursor
// for the same account, query, and order.
func DecodeWhatsAppChatCursor(filter WhatsAppChatFilter, value string) (WhatsAppChatCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) > 256 {
		return WhatsAppChatCursor{}, ErrWhatsAppChatCursorInvalid
	}
	var wire whatsAppChatCursorWire
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil ||
		wire.Version != whatsAppChatCursorVersion || wire.ID <= 0 ||
		wire.Filter != whatsAppChatFilterKey(filter) {
		return WhatsAppChatCursor{}, ErrWhatsAppChatCursorInvalid
	}
	cursor := WhatsAppChatCursor{ConversationID: wire.ID}
	if wire.Epoch != nil {
		cursor.HasActivity = true
		cursor.ActivityEpoch = *wire.Epoch
	}
	return cursor, nil
}

// unicodeLowerExpr lowercases a text expression with full Unicode folding on
// both backends. SQLite's built-in LOWER folds ASCII only.
func (s *Store) unicodeLowerExpr(expr string) string {
	if s.IsPostgreSQL() {
		return "LOWER(" + expr + ")"
	}
	return sqliteutil.UnicodeLowerFunction + "(" + expr + ")"
}

// whatsAppActivityEpochExpr converts a timestamp column to integer unix
// seconds on both backends. SQLite stores timestamps as text that may carry
// mixed UTC offsets, so the ordering and the keyset predicate compare
// integers rather than text.
func (s *Store) whatsAppActivityEpochExpr(column string) string {
	if s.IsPostgreSQL() {
		return "FLOOR(EXTRACT(EPOCH FROM " + column + "))::BIGINT"
	}
	return "CAST(strftime('%s', " + column + ") AS INTEGER)"
}

// ListWhatsAppChats returns one page of WhatsApp conversations in the
// filter's order.
func (s *Store) ListWhatsAppChats(ctx context.Context, filter WhatsAppChatFilter) (WhatsAppChatPage, error) {
	order := whatsAppChatOrder(filter)
	if order != WhatsAppChatOrderRecent && order != WhatsAppChatOrderCreated {
		return WhatsAppChatPage{}, ErrWhatsAppChatOrderInvalid
	}
	limit := filter.Limit
	if limit <= 0 || limit > MaxWhatsAppChatPage {
		limit = MaxWhatsAppChatPage
	}
	activity := s.whatsAppActivityEpochExpr("c.last_message_at")
	query := `
		SELECT c.id, src.identifier, c.source_conversation_id,
		       c.title, c.conversation_type, c.message_count,
		       c.last_message_at, c.last_message_preview,
		       ` + activity + `
		FROM conversations c
		JOIN sources src ON src.id = c.source_id
		WHERE src.source_type = ?
		  AND c.source_conversation_id IS NOT NULL`
	args := []any{WhatsAppSourceType}
	if filter.Account != "" {
		query += ` AND src.identifier = ?`
		args = append(args, filter.Account)
	}
	if term := whatsAppChatQueryTerm(filter); term != "" {
		pattern := "%" + escapeLike(term) + "%"
		query += ` AND (` + s.unicodeLowerExpr("COALESCE(c.title, '')") + ` LIKE ? ESCAPE '\'` +
			` OR ` + s.unicodeLowerExpr("c.source_conversation_id") + ` LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern)
	}
	switch after := filter.After; {
	case order == WhatsAppChatOrderCreated:
		if after != nil {
			query += ` AND c.id > ?`
			args = append(args, after.ConversationID)
		}
		query += ` ORDER BY c.id ASC LIMIT ?`
	case after == nil:
		query += ` ORDER BY (` + activity + ` IS NULL), ` + activity + ` DESC, c.id DESC LIMIT ?`
	case after.HasActivity:
		query += ` AND (` + activity + ` IS NULL OR ` + activity + ` < ?` +
			` OR (` + activity + ` = ? AND c.id < ?))` +
			` ORDER BY (` + activity + ` IS NULL), ` + activity + ` DESC, c.id DESC LIMIT ?`
		args = append(args, after.ActivityEpoch, after.ActivityEpoch, after.ConversationID)
	default:
		query += ` AND ` + activity + ` IS NULL AND c.id < ?` +
			` ORDER BY (` + activity + ` IS NULL), ` + activity + ` DESC, c.id DESC LIMIT ?`
		args = append(args, after.ConversationID)
	}
	// One extra row decides HasMore without a count query.
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return WhatsAppChatPage{}, fmt.Errorf("list whatsapp chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var chats []WhatsAppChatSummary
	var epochs []sql.NullInt64
	for rows.Next() {
		var c WhatsAppChatSummary
		var epoch sql.NullInt64
		if err := rows.Scan(
			&c.ConversationID, &c.Account, &c.ChatJID,
			&c.Title, &c.ConversationType, &c.MessageCount,
			&c.LastMessageAt, &c.LastMessagePreview, &epoch,
		); err != nil {
			return WhatsAppChatPage{}, fmt.Errorf("scan whatsapp chat: %w", err)
		}
		chats = append(chats, c)
		epochs = append(epochs, epoch)
	}
	if err := rows.Err(); err != nil {
		return WhatsAppChatPage{}, fmt.Errorf("list whatsapp chats: %w", err)
	}
	page := WhatsAppChatPage{Chats: chats}
	if len(chats) > limit {
		last := limit - 1
		page.Chats = chats[:limit]
		page.HasMore = true
		page.Next = WhatsAppChatCursor{ConversationID: chats[last].ConversationID}
		if order == WhatsAppChatOrderRecent {
			page.Next.ActivityEpoch = epochs[last].Int64
			page.Next.HasActivity = epochs[last].Valid
		}
	}
	return page, nil
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

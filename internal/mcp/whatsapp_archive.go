package mcp

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/store"
)

// WhatsApp-scoped archive read tools. They read only WhatsApp conversations
// and messages, so a WhatsApp-only MCP endpoint can browse chats and resolve
// message ids for reactions without exposing the general archive catalog.
const (
	defaultWhatsAppChatLimit    = 100
	maxWhatsAppChatLimit        = 500
	defaultWhatsAppMessageLimit = 100
	maxWhatsAppMessageLimit     = store.MaxWhatsAppMessagePage

	toolArgChatJID = "chat_jid"
	toolArgAfterID = "after_id"
	toolArgOrder   = "order"
)

type whatsAppChat struct {
	ConversationID     int64      `json:"conversation_id"`
	Account            string     `json:"account"`
	ChatJID            string     `json:"chat_jid"`
	Title              string     `json:"title,omitempty"`
	ConversationType   string     `json:"conversation_type"`
	MessageCount       int64      `json:"message_count"`
	LastMessageAt      *time.Time `json:"last_message_at,omitempty"`
	LastMessagePreview string     `json:"last_message_preview,omitempty"`
}

type listWhatsAppChatsResponse struct {
	Chats []whatsAppChat `json:"chats"`
	// HasMore reports that older chats follow; NextCursor then resumes after
	// the last chat of this page.
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type whatsAppArchivedMessage struct {
	ID              int64      `json:"id"`
	Account         string     `json:"account"`
	ChatJID         string     `json:"chat_jid"`
	SourceMessageID string     `json:"source_message_id"`
	SenderJID       string     `json:"sender_jid,omitempty"`
	SenderName      string     `json:"sender_name,omitempty"`
	IsFromMe        bool       `json:"is_from_me"`
	SentAt          *time.Time `json:"sent_at,omitempty"`
	Body            string     `json:"body,omitempty"`
}

type listWhatsAppMessagesResponse struct {
	Messages []whatsAppArchivedMessage `json:"messages"`
	// NextAfterID is the after_id to pass for the next page. It equals the
	// requested after_id when the page is empty.
	NextAfterID int64 `json:"next_after_id"`
}

func listWhatsAppChatsDefinition() toolDefinition {
	return readDefinition(
		ToolListWhatsAppChats,
		"List archived WhatsApp chats. Returns the chat_jid accepted by list_whatsapp_messages and send_whatsapp_message. "+
			"Page with cursor while has_more is true; narrow large archives with query. "+
			"order=recent (default) lists the most recently active chat first and is best-effort when paged: "+
			"a chat that gains activity while you page moves to the top and is not revisited, so re-list the first page to catch it. "+
			"order=created walks chats by immutable conversation id, oldest first, and visits every chat that existed when the walk began.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgAccount: stringSchema("Only chats of this WhatsApp account identifier; omit for every archived account"),
			toolArgQuery:   stringSchema("Case-insensitive substring of the chat title or chat JID"),
			toolArgLimit:   nonNegativeIntegerSchema("Maximum chats per page (1-500, default 100)", defaultWhatsAppChatLimit),
			toolArgOrder:   stringSchema("recent: most recent activity first (default); created: ascending conversation id, exhaustive", string(store.WhatsAppChatOrderRecent), string(store.WhatsAppChatOrderCreated)),
			toolArgCursor:  stringSchema("Opaque next_cursor from the previous page; requires the same account, query, and order"),
		}),
		outputSchemaFor[listWhatsAppChatsResponse](),
		(*handlers).listWhatsAppChats,
	)
}

func listWhatsAppMessagesDefinition() toolDefinition {
	return readDefinition(
		ToolListWhatsAppMessages,
		"List archived messages of one WhatsApp chat in ascending archive id order, "+
			"which is the order they were archived and is stable for incremental polling: "+
			"page forward by passing the returned next_after_id as after_id. "+
			"After a history backfill older messages can carry higher ids, so sort by sent_at for chronological display. "+
			"Pass account when more than one WhatsApp account is archived, since accounts can share a chat JID. "+
			"Message ids are accepted by send_whatsapp_reaction.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgChatJID: stringSchema("WhatsApp chat JID from list_whatsapp_chats"),
			toolArgAccount: stringSchema("Only messages of this WhatsApp account identifier; omit for every archived account"),
			toolArgAfterID: nonNegativeIntegerSchema("Only messages with an id greater than this (default 0)", 0),
			toolArgLimit:   nonNegativeIntegerSchema("Maximum messages per page (1-500, default 100)", defaultWhatsAppMessageLimit),
		}, toolArgChatJID),
		outputSchemaFor[listWhatsAppMessagesResponse](),
		(*handlers).listWhatsAppMessages,
	)
}

func (h *handlers) listWhatsAppChats(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.whatsAppArchive == nil {
		return toolErrorResult("WhatsApp archive not configured"), nil
	}
	args := req.GetArguments()
	filter := store.WhatsAppChatFilter{
		Account: trimmedStringArg(args, toolArgAccount),
		Query:   trimmedStringArg(args, toolArgQuery),
		Limit:   boundedIntArg(args, toolArgLimit, defaultWhatsAppChatLimit, maxWhatsAppChatLimit),
		Order:   store.WhatsAppChatOrder(trimmedStringArg(args, toolArgOrder)),
	}
	switch filter.Order {
	case "", store.WhatsAppChatOrderRecent, store.WhatsAppChatOrderCreated:
	default:
		return toolErrorResult("invalid_order: order must be recent or created"), nil
	}
	if token := trimmedStringArg(args, toolArgCursor); token != "" {
		cursor, err := store.DecodeWhatsAppChatCursor(filter, token)
		if err != nil {
			return toolErrorResult("invalid_cursor: cursor is malformed or was issued for a different account, query, or order"), nil
		}
		filter.After = &cursor
	}
	page, err := h.whatsAppArchive.ListWhatsAppChats(ctx, filter)
	if err != nil {
		return nil, newInternalError("list whatsapp chats", err)
	}
	response := listWhatsAppChatsResponse{
		Chats:   make([]whatsAppChat, 0, len(page.Chats)),
		HasMore: page.HasMore,
	}
	if page.HasMore {
		response.NextCursor = store.EncodeWhatsAppChatCursor(filter, page.Next)
	}
	for _, chat := range page.Chats {
		item := whatsAppChat{
			ConversationID:     chat.ConversationID,
			Account:            chat.Account,
			ChatJID:            chat.ChatJID,
			Title:              chat.Title.String,
			ConversationType:   chat.ConversationType,
			MessageCount:       chat.MessageCount,
			LastMessagePreview: chat.LastMessagePreview.String,
		}
		if chat.LastMessageAt.Valid {
			lastMessageAt := chat.LastMessageAt.Time
			item.LastMessageAt = &lastMessageAt
		}
		response.Chats = append(response.Chats, item)
	}
	return jsonResult(response)
}

func (h *handlers) listWhatsAppMessages(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.whatsAppArchive == nil {
		return toolErrorResult("WhatsApp archive not configured"), nil
	}
	args := req.GetArguments()
	chatJID := trimmedStringArg(args, toolArgChatJID)
	if chatJID == "" {
		return toolErrorResult("chat_jid is required"), nil
	}
	afterID, toolErr := nonNegativeInt64Arg(args, toolArgAfterID)
	if toolErr != nil {
		return toolErr, nil
	}
	limit := boundedIntArg(args, toolArgLimit, defaultWhatsAppMessageLimit, maxWhatsAppMessageLimit)
	messages, err := h.whatsAppArchive.ListWhatsAppMessagesAfter(ctx, store.WhatsAppMessageFilter{
		Account: trimmedStringArg(args, toolArgAccount),
		ChatJID: chatJID,
		AfterID: afterID,
		Limit:   limit,
	})
	if err != nil {
		return nil, newInternalError("list whatsapp messages", err)
	}
	response := listWhatsAppMessagesResponse{
		Messages:    make([]whatsAppArchivedMessage, 0, len(messages)),
		NextAfterID: afterID,
	}
	for _, message := range messages {
		item := whatsAppArchivedMessage{
			ID:              message.ID,
			Account:         message.Account,
			ChatJID:         message.ChatJID,
			SourceMessageID: message.SourceMessageID,
			SenderJID:       message.SenderJID,
			SenderName:      message.SenderName.String,
			IsFromMe:        message.IsFromMe,
			Body:            message.Body.String,
		}
		if message.SentAt.Valid {
			sentAt := message.SentAt.Time
			item.SentAt = &sentAt
		}
		response.Messages = append(response.Messages, item)
		response.NextAfterID = message.ID
	}
	return jsonResult(response)
}

func trimmedStringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

// nonNegativeInt64Arg reads an optional integer argument, rejecting negative,
// fractional, and unsafe values. A missing argument reads as zero.
func nonNegativeInt64Arg(args map[string]any, key string) (int64, *toolResult) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return 0, nil
	}
	value, ok := raw.(float64)
	if !ok || math.IsNaN(value) || value < 0 || value > maxJSONSafeInteger || value != math.Trunc(value) {
		return 0, toolErrorResult("invalid_" + key + ": " + key + " must be a non-negative integer")
	}
	return int64(value), nil
}

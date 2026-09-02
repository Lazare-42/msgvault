package mcp

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// WhatsApp-scoped archive read tools. They read only WhatsApp conversations
// and messages, so a WhatsApp-only MCP endpoint can browse chats and resolve
// message ids for reactions without exposing the general archive catalog.
const (
	defaultWhatsAppChatLimit    = 100
	maxWhatsAppChatLimit        = 1000
	defaultWhatsAppMessageLimit = 100
	maxWhatsAppMessageLimit     = 500

	toolArgChatJID = "chat_jid"
	toolArgAfterID = "after_id"
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
}

type whatsAppArchivedMessage struct {
	ID              int64      `json:"id"`
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
		"List archived WhatsApp chats, most recent activity first. "+
			"Returns the chat_jid accepted by list_whatsapp_messages and send_whatsapp_message.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgLimit: nonNegativeIntegerSchema("Maximum chats (1-1000, default 100)", defaultWhatsAppChatLimit),
		}),
		outputSchemaFor[listWhatsAppChatsResponse](),
		(*handlers).listWhatsAppChats,
	)
}

func listWhatsAppMessagesDefinition() toolDefinition {
	return readDefinition(
		ToolListWhatsAppMessages,
		"List archived messages of one WhatsApp chat, oldest first. "+
			"Page forward by passing the returned next_after_id as after_id. "+
			"Message ids are accepted by send_whatsapp_reaction.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgChatJID: stringSchema("WhatsApp chat JID from list_whatsapp_chats"),
			toolArgAfterID: nonNegativeIntegerSchema("Only messages with an id greater than this (default 0)", 0),
			toolArgLimit:   nonNegativeIntegerSchema("Maximum messages (1-500, default 100)", defaultWhatsAppMessageLimit),
		}, toolArgChatJID),
		outputSchemaFor[listWhatsAppMessagesResponse](),
		(*handlers).listWhatsAppMessages,
	)
}

func (h *handlers) listWhatsAppChats(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.whatsAppArchive == nil {
		return toolErrorResult("WhatsApp archive not configured"), nil
	}
	limit := boundedIntArg(req.GetArguments(), toolArgLimit, defaultWhatsAppChatLimit, maxWhatsAppChatLimit)
	chats, err := h.whatsAppArchive.ListWhatsAppChats(ctx, limit)
	if err != nil {
		return nil, newInternalError("list whatsapp chats", err)
	}
	response := listWhatsAppChatsResponse{Chats: make([]whatsAppChat, 0, len(chats))}
	for _, chat := range chats {
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
	chatJID, _ := args[toolArgChatJID].(string)
	chatJID = strings.TrimSpace(chatJID)
	if chatJID == "" {
		return toolErrorResult("chat_jid is required"), nil
	}
	afterID, toolErr := nonNegativeInt64Arg(args, toolArgAfterID)
	if toolErr != nil {
		return toolErr, nil
	}
	limit := boundedIntArg(args, toolArgLimit, defaultWhatsAppMessageLimit, maxWhatsAppMessageLimit)
	messages, err := h.whatsAppArchive.ListWhatsAppMessagesAfter(ctx, chatJID, afterID, limit)
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

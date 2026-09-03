package cmd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

type whatsappMessageSender interface {
	SendMessage(ctx context.Context, req whatsapplive.SendMessageRequest) (whatsapplive.SendResult, error)
}

// whatsappLiveAPIHandler exposes backfill and send endpoints for trusted local
// consumers (chat bridge, agents). Disabled unless an API token is set; callers
// authenticate with Authorization: Bearer <token>.
type whatsappLiveAPIHandler struct {
	store  *store.Store
	sender whatsappMessageSender
	token  string
}

type whatsappAPIChat struct {
	ConversationID     int64      `json:"conversation_id"`
	Account            string     `json:"account"`
	ChatJID            string     `json:"chat_jid"`
	Title              string     `json:"title,omitempty"`
	ConversationType   string     `json:"conversation_type"`
	MessageCount       int64      `json:"message_count"`
	LastMessageAt      *time.Time `json:"last_message_at,omitempty"`
	LastMessagePreview string     `json:"last_message_preview,omitempty"`
}

type whatsappAPIMessage struct {
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

func (h *whatsappLiveAPIHandler) authorized(r *http.Request) bool {
	if h.token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

func (h *whatsappLiveAPIHandler) guard(w http.ResponseWriter, r *http.Request, method string) bool {
	if h.token == "" {
		http.Error(w, "whatsapp live API is disabled; set MSGVAULT_WHATSAPP_API_TOKEN", http.StatusNotFound)
		return false
	}
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (h *whatsappLiveAPIHandler) listChats(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	filter := store.WhatsAppChatFilter{
		Account: strings.TrimSpace(q.Get("account")),
		Query:   q.Get("q"),
		Limit:   limit,
		Order:   store.WhatsAppChatOrder(strings.TrimSpace(q.Get("order"))),
	}
	if token := strings.TrimSpace(q.Get("cursor")); token != "" {
		cursor, err := store.DecodeWhatsAppChatCursor(filter, token)
		if err != nil {
			http.Error(w, "invalid cursor for this account, q, and order", http.StatusBadRequest)
			return
		}
		filter.After = &cursor
	}
	page, err := h.store.ListWhatsAppChats(r.Context(), filter)
	if errors.Is(err, store.ErrWhatsAppChatOrderInvalid) {
		http.Error(w, "order must be recent or created", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]whatsappAPIChat, 0, len(page.Chats))
	for _, c := range page.Chats {
		item := whatsappAPIChat{
			ConversationID:     c.ConversationID,
			Account:            c.Account,
			ChatJID:            c.ChatJID,
			Title:              c.Title.String,
			ConversationType:   c.ConversationType,
			MessageCount:       c.MessageCount,
			LastMessagePreview: c.LastMessagePreview.String,
		}
		if c.LastMessageAt.Valid {
			t := c.LastMessageAt.Time
			item.LastMessageAt = &t
		}
		out = append(out, item)
	}
	response := map[string]any{"chats": out, "has_more": page.HasMore}
	if page.HasMore {
		response["next_cursor"] = store.EncodeWhatsAppChatCursor(filter, page.Next)
	}
	writeAPIJSON(w, response)
}

func (h *whatsappLiveAPIHandler) listMessages(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	chatJID := strings.TrimSpace(q.Get("chat_jid"))
	if chatJID == "" {
		http.Error(w, "chat_jid is required", http.StatusBadRequest)
		return
	}
	afterID, _ := strconv.ParseInt(q.Get("after_id"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	messages, err := h.store.ListWhatsAppMessagesAfter(r.Context(), store.WhatsAppMessageFilter{
		Account: strings.TrimSpace(q.Get("account")),
		ChatJID: chatJID,
		AfterID: afterID,
		Limit:   limit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]whatsappAPIMessage, 0, len(messages))
	for _, m := range messages {
		item := whatsappAPIMessage{
			ID:              m.ID,
			Account:         m.Account,
			ChatJID:         m.ChatJID,
			SourceMessageID: m.SourceMessageID,
			SenderJID:       m.SenderJID,
			SenderName:      m.SenderName.String,
			IsFromMe:        m.IsFromMe,
			Body:            m.Body.String,
		}
		if m.SentAt.Valid {
			t := m.SentAt.Time
			item.SentAt = &t
		}
		out = append(out, item)
	}
	nextCursor := afterID
	if len(messages) > 0 {
		nextCursor = messages[len(messages)-1].ID
	}
	writeAPIJSON(w, map[string]any{"messages": out, "next_after_id": nextCursor})
}

type whatsappAPISendRequest struct {
	Account        string   `json:"account,omitempty"`
	ChatID         string   `json:"chat_id"`
	Body           string   `json:"body"`
	LocalRequestID string   `json:"local_request_id,omitempty"`
	Mentions       []string `json:"mentions,omitempty"`
}

func (h *whatsappLiveAPIHandler) sendMessage(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, http.MethodPost) {
		return
	}
	if h.sender == nil {
		http.Error(w, "whatsapp sender is unavailable", http.StatusServiceUnavailable)
		return
	}

	var req whatsappAPISendRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	result, err := h.sender.SendMessage(r.Context(), whatsapplive.SendMessageRequest{
		Account:        req.Account,
		ChatID:         req.ChatID,
		Body:           req.Body,
		LocalRequestID: req.LocalRequestID,
		Mentions:       req.Mentions,
	})
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "is required") {
			status = http.StatusBadRequest
		} else if strings.Contains(err.Error(), "not ready") {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}

	writeAPIJSON(w, result)
}

func writeAPIJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

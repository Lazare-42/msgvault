package cmd

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// whatsappLiveAPIHandler exposes read-only backfill endpoints for local
// consumers (chat bridge, agents). Disabled unless an API token is set;
// callers authenticate with Authorization: Bearer <token>.
type whatsappLiveAPIHandler struct {
	store *store.Store
	token string
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

func (h *whatsappLiveAPIHandler) guard(w http.ResponseWriter, r *http.Request) bool {
	if h.token == "" {
		http.Error(w, "whatsapp live API is disabled; set MSGVAULT_WHATSAPP_API_TOKEN", http.StatusNotFound)
		return false
	}
	if r.Method != http.MethodGet {
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
	if !h.guard(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	chats, err := h.store.ListWhatsAppChats(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]whatsappAPIChat, 0, len(chats))
	for _, c := range chats {
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
	writeAPIJSON(w, map[string]any{"chats": out})
}

func (h *whatsappLiveAPIHandler) listMessages(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
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
	messages, err := h.store.ListWhatsAppMessagesAfter(r.Context(), chatJID, afterID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]whatsappAPIMessage, 0, len(messages))
	for _, m := range messages {
		item := whatsappAPIMessage{
			ID:              m.ID,
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

func writeAPIJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

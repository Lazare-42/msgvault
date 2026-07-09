package live

import (
	"context"
	"time"
)

type Client interface {
	Status(ctx context.Context) (Status, error)
	Connect(ctx context.Context) error
	Close() error
	Logout(ctx context.Context, req LogoutRequest) (LogoutResult, error)
	SendMessage(ctx context.Context, req SendMessageRequest) (SendResult, error)
	SendReaction(ctx context.Context, req SendReactionRequest) (SendResult, error)
}

type Status struct {
	Account     string `json:"account,omitempty"`
	AccountJID  string `json:"account_jid,omitempty"`
	Connected   bool   `json:"connected"`
	LoggedIn    bool   `json:"logged_in"`
	Paired      bool   `json:"paired"`
	SessionPath string `json:"session_path,omitempty"`
}

type QRPairingState struct {
	Active    bool      `json:"active"`
	Code      string    `json:"code,omitempty"`
	Event     string    `json:"event,omitempty"`
	Error     string    `json:"error,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Paired    bool      `json:"paired"`
}

type LoginState struct {
	Status  Status         `json:"status"`
	Pairing QRPairingState `json:"pairing"`
}

type SendMessageRequest struct {
	Account        string
	ChatID         string
	Body           string
	LocalRequestID string
}

type LogoutRequest struct {
	Account    string
	ForceLocal bool
}

type LogoutResult struct {
	StatusBefore        Status `json:"status_before"`
	StatusAfter         Status `json:"status_after"`
	RemoteLogout        bool   `json:"remote_logout"`
	LocalSessionCleared bool   `json:"local_session_cleared"`
	ForcedLocalClear    bool   `json:"forced_local_clear"`
}

type SendReactionRequest struct {
	Account        string
	MessageID      int64
	Emoji          string
	LocalRequestID string
}

type SendResult struct {
	LocalRequestID  string `json:"local_request_id"`
	OutboxID        int64  `json:"outbox_id,omitempty"`
	MessageID       int64  `json:"message_id,omitempty"`
	RemoteMessageID string `json:"remote_message_id,omitempty"`
	ChatJID         string `json:"chat_jid,omitempty"`
	Status          string `json:"status"`
}

type InboundMessage struct {
	Account   string
	ChatJID   string
	SenderJID string
	MessageID string
	PushName  string
	Text      string
	Timestamp time.Time
	IsFromMe  bool
	IsGroup   bool
	RawJSON   []byte
	Reaction  *InboundReaction
}

type InboundReaction struct {
	TargetChatJID   string
	TargetMessageID string
	TargetSenderJID string
	Emoji           string
	TargetFromMe    bool
}

type Transport interface {
	Status(ctx context.Context) (Status, error)
	Connect(ctx context.Context) error
	Close() error
	Logout(ctx context.Context, req TransportLogoutRequest) (TransportLogoutResult, error)
	SendMessage(ctx context.Context, req TransportSendMessageRequest) (TransportSendResult, error)
	SendReaction(ctx context.Context, req TransportSendReactionRequest) (TransportSendResult, error)
}

type TransportLogoutRequest struct {
	ForceLocal bool
}

type TransportLogoutResult struct {
	RemoteLogout        bool
	LocalSessionCleared bool
	ForcedLocalClear    bool
}

type TransportSendMessageRequest struct {
	Account string
	ChatID  string
	Body    string
}

type TransportSendReactionRequest struct {
	Account         string
	ChatJID         string
	SenderJID       string
	RemoteMessageID string
	Emoji           string
}

type TransportSendResult struct {
	RemoteMessageID string
	ChatJID         string
	Timestamp       time.Time
}

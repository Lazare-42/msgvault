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
	RequestHistorySync(ctx context.Context, req RequestHistorySyncRequest) (RequestHistorySyncResult, error)
}

// DefaultHistorySyncRequestCount is WhatsApp's own documented recommendation
// (see whatsmeow's Client.BuildHistorySyncRequest) for how many messages to
// request per on-demand history-sync call.
const DefaultHistorySyncRequestCount = 50

// MaxHistorySyncRequestCount caps how many messages a single on-demand
// history-sync request can ask for. WhatsApp's protocol does not document a
// hard maximum; this only guards against one call asking for an unreasonably
// large batch — the documented recommendation is DefaultHistorySyncRequestCount.
const MaxHistorySyncRequestCount = 100

type Status struct {
	Account     string `json:"account,omitempty"`
	AccountJID  string `json:"account_jid,omitempty"`
	Connected   bool   `json:"connected"`
	LoggedIn    bool   `json:"logged_in"`
	Paired      bool   `json:"paired"`
	Ready       bool   `json:"ready"`
	SessionPath string `json:"session_path,omitempty"`
}

func (s Status) IsReady() bool {
	return s.Paired && s.Connected && s.LoggedIn
}

func (s *Status) ApplyDerived() {
	s.Ready = s.IsReady()
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
	// Mentions is a list of full JID strings (e.g. "178357123686403@lid" or
	// "33612345678@s.whatsapp.net") to @mention. The Body must contain a
	// matching "@<user>" token per JID for WhatsApp to render the name + ping.
	Mentions []string
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

// RequestHistorySyncRequest asks WhatsApp's own on-demand history-sync
// mechanism (whatsmeow's BuildHistorySyncRequest, sent as a peer/protocol
// message to the primary device) for more messages older than the oldest
// message msgvault has already archived for ChatID. This is best-effort and
// asynchronous: WhatsApp's servers/primary device decide whether to honor
// it, and — when they do — the response arrives later as a normal
// *events.HistorySync (archived by the existing history-sync handler), not
// as a direct reply to this call. There is no guarantee the requested
// messages still exist on WhatsApp's side.
type RequestHistorySyncRequest struct {
	Account string
	ChatID  string
	// Count is how many messages to request. Defaults to
	// DefaultHistorySyncRequestCount and is capped at
	// MaxHistorySyncRequestCount.
	Count int
}

// RequestHistorySyncResult describes what was requested, not what came back
// — see RequestHistorySyncRequest's doc comment on the asynchronous,
// best-effort nature of this operation.
type RequestHistorySyncResult struct {
	ChatJID         string    `json:"chat_jid"`
	AnchorMessageID string    `json:"anchor_message_id"`
	AnchorTimestamp time.Time `json:"anchor_timestamp"`
	AnchorIsFromMe  bool      `json:"anchor_is_from_me"`
	RequestedCount  int       `json:"requested_count"`
}

type InboundMessage struct {
	Account   string
	ChatJID   string
	ChatTitle string
	SenderJID string
	MessageID string
	PushName  string
	Text      string
	Timestamp time.Time
	IsFromMe  bool
	IsGroup   bool
	RawJSON   []byte
	Reaction  *InboundReaction
	// Attachment is non-nil when the message carries a downloadable media
	// payload (image/video/document/audio/sticker), even if the caption is
	// empty. A non-nil Attachment with empty Data means the media reference
	// existed but the bytes could not be downloaded (see DownloadError); the
	// message is archived regardless so it is never silently dropped.
	Attachment *InboundAttachment
}

// InboundAttachment captures a WhatsApp media payload discovered on an
// inbound message. Data is only populated when the bytes were downloaded
// successfully; decryption requires whatsmeow's live event context, so this
// must happen where the *events.Message is first observed (registerEventHandler
// / archiveHistorySync), not later from stored metadata alone.
type InboundAttachment struct {
	Filename  string
	MimeType  string
	MediaType string // image, video, document, audio, voice_note, sticker
	Size      int64
	Data      []byte
	// DownloadError explains why Data is empty (e.g. expired media key,
	// network failure). Empty when Data was downloaded successfully.
	DownloadError string
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
	Account  string
	ChatID   string
	Body     string
	Mentions []string // full JID strings to @mention (see SendMessageRequest)
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

// TransportRequestHistorySyncRequest carries an already-resolved anchor
// message (the caller — Service.RequestHistorySync — is responsible for
// finding it) down to the transport, which builds and sends the actual
// whatsmeow on-demand history-sync protocol message.
type TransportRequestHistorySyncRequest struct {
	ChatJID         string
	AnchorMessageID string
	AnchorTimestamp time.Time
	AnchorIsFromMe  bool
	Count           int
}

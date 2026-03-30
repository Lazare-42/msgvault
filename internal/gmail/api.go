// Package gmail provides a Gmail API client with rate limiting and retry logic.
package gmail

import "context"

// AccountReader provides read access to account-level Gmail data.
type AccountReader interface {
	// GetProfile returns the authenticated user's profile.
	GetProfile(ctx context.Context) (*Profile, error)

	// ListLabels returns all labels for the account.
	ListLabels(ctx context.Context) ([]*Label, error)
}

// MessageReader provides read access to Gmail messages and history.
type MessageReader interface {
	// ListMessages returns message IDs matching the query.
	// Use pageToken for pagination. Returns next page token if more results exist.
	ListMessages(ctx context.Context, query string, pageToken string) (*MessageListResponse, error)

	// GetMessageRaw fetches a single message with raw MIME data.
	GetMessageRaw(ctx context.Context, messageID string) (*RawMessage, error)

	// GetMessagesRawBatch fetches multiple messages in parallel with rate limiting.
	// Returns results in the same order as input IDs. Failed fetches return nil.
	GetMessagesRawBatch(ctx context.Context, messageIDs []string) ([]*RawMessage, error)

	// ListHistory returns changes since the given history ID.
	ListHistory(ctx context.Context, startHistoryID uint64, pageToken string) (*HistoryResponse, error)
}

// MessageDeleter provides write operations for deleting Gmail messages.
type MessageDeleter interface {
	// TrashMessage moves a message to trash (recoverable for 30 days).
	TrashMessage(ctx context.Context, messageID string) error

	// DeleteMessage permanently deletes a message.
	DeleteMessage(ctx context.Context, messageID string) error

	// BatchDeleteMessages permanently deletes multiple messages (max 1000).
	BatchDeleteMessages(ctx context.Context, messageIDs []string) error
}

// DraftManager provides CRUD operations for Gmail drafts.
type DraftManager interface {
	// ListDrafts returns drafts matching the optional query.
	ListDrafts(ctx context.Context, query string, maxResults int) ([]*Draft, error)

	// GetDraft returns a single draft by ID.
	GetDraft(ctx context.Context, draftID string) (*Draft, error)

	// CreateDraft creates a new draft. If threadID is non-empty, the draft
	// is created as a reply within that thread.
	CreateDraft(ctx context.Context, draft *DraftCompose) (*Draft, error)

	// UpdateDraft replaces the content of an existing draft.
	UpdateDraft(ctx context.Context, draftID string, draft *DraftCompose) (*Draft, error)

	// DeleteDraft permanently deletes a draft.
	DeleteDraft(ctx context.Context, draftID string) error

	// SendDraft sends a draft. The draft is removed from drafts and a
	// message is created in the sent folder.
	SendDraft(ctx context.Context, draftID string) (*SentMessage, error)
}

// LabelManager provides operations for modifying Gmail labels on messages.
type LabelManager interface {
	// ModifyMessageLabels adds and/or removes labels on a single message.
	ModifyMessageLabels(ctx context.Context, messageID string, addLabelIDs, removeLabelIDs []string) error

	// BatchModifyLabels adds and/or removes labels on multiple messages (max 1000).
	BatchModifyLabels(ctx context.Context, messageIDs, addLabelIDs, removeLabelIDs []string) error

	// CreateLabel creates a new user label and returns it.
	CreateLabel(ctx context.Context, name string) (*Label, error)
}

// API defines the interface for Gmail operations.
// This interface enables mocking for tests without hitting the real API.
type API interface {
	AccountReader
	MessageReader
	MessageDeleter
	DraftManager
	LabelManager

	// Close releases any resources held by the client.
	Close() error
}

// Profile represents a Gmail user profile.
type Profile struct {
	EmailAddress  string
	MessagesTotal int64
	ThreadsTotal  int64
	HistoryID     uint64
}

// Label represents a Gmail label.
type Label struct {
	ID                    string
	Name                  string
	Type                  string // "system" or "user"
	MessagesTotal         int64
	MessagesUnread        int64
	MessageListVisibility string
	LabelListVisibility   string
}

// MessageListResponse contains a page of message IDs.
type MessageListResponse struct {
	Messages           []MessageID
	NextPageToken      string
	ResultSizeEstimate int64
}

// MessageID represents a message reference from list operations.
type MessageID struct {
	ID       string
	ThreadID string
}

// RawMessage contains the raw MIME data for a message.
type RawMessage struct {
	ID           string
	ThreadID     string
	LabelIDs     []string
	Snippet      string
	HistoryID    uint64
	InternalDate int64 // Unix milliseconds
	SizeEstimate int64
	Raw          []byte // Decoded from base64url
}

// RawMessageBatchResult is one per-message result from a batch raw fetch.
// Message is nil when the fetch failed; Err preserves the per-message cause.
type RawMessageBatchResult struct {
	ID      string
	Message *RawMessage
	Err     error
}

// MessageLabelsBatchResult is one per-message result from a batch label fetch.
// LabelIDs is nil when the fetch failed; Err preserves the per-message cause.
type MessageLabelsBatchResult struct {
	ID              string
	LabelIDs        []string
	RFC822MessageID string
	Err             error
}

// HistoryResponse contains changes since a history ID.
type HistoryResponse struct {
	History       []HistoryRecord
	NextPageToken string
	HistoryID     uint64
}

// HistoryRecord represents a single history change.
type HistoryRecord struct {
	ID              uint64
	MessagesAdded   []HistoryMessage
	MessagesDeleted []HistoryMessage
	LabelsAdded     []HistoryLabelChange
	LabelsRemoved   []HistoryLabelChange
}

// HistoryMessage represents a message in history.
type HistoryMessage struct {
	Message MessageID
}

// HistoryLabelChange represents a label change in history.
type HistoryLabelChange struct {
	Message  MessageID
	LabelIDs []string
}

// Draft represents a Gmail draft with its message content.
type Draft struct {
	ID      string // Draft ID (used for update/send/delete)
	Message DraftMessage
}

// DraftMessage contains the parsed fields of a draft's message.
type DraftMessage struct {
	ID       string // Message ID
	ThreadID string
	LabelIDs []string
	Snippet  string
	From     string
	To       []string
	Cc       []string
	Bcc      []string
	Subject  string
	Body     string // Plain text body (or HTML if no plain text)
	Date     int64  // Unix milliseconds
}

// DraftCompose holds the fields for creating or updating a draft.
type DraftCompose struct {
	To          []string
	Cc          []string
	Bcc         []string
	Subject     string
	Body        string // Email body (plain text or HTML)
	ContentType string // "text/plain" (default) or "text/html"
	ThreadID    string // Optional: set to reply within a thread
}

// SentMessage contains the result of sending a draft.
type SentMessage struct {
	ID       string
	ThreadID string
	LabelIDs []string
}

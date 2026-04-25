package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

const (
	baseURL        = "https://gmail.googleapis.com/gmail/v1"
	maxRetries     = 12  // Covers ~10 minutes of network outages
	maxBackoff     = 600 // Max backoff in seconds
	defaultTimeout = 30 * time.Second
)

// Client implements the Gmail API interface.
type Client struct {
	httpClient  *http.Client
	rateLimiter *RateLimiter
	logger      *slog.Logger
	userID      string // "me" for authenticated user
	concurrency int    // Max parallel requests for batch operations
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithLogger sets the logger for the client.
func WithLogger(logger *slog.Logger) ClientOption {
	return func(c *Client) {
		c.logger = logger
	}
}

// WithConcurrency sets the max concurrent requests for batch operations.
func WithConcurrency(n int) ClientOption {
	return func(c *Client) {
		c.concurrency = n
	}
}

// WithRateLimiter sets a custom rate limiter.
func WithRateLimiter(rl *RateLimiter) ClientOption {
	return func(c *Client) {
		c.rateLimiter = rl
	}
}

// NewClient creates a new Gmail API client.
func NewClient(tokenSource oauth2.TokenSource, opts ...ClientOption) *Client {
	c := &Client{
		httpClient:  oauth2.NewClient(context.Background(), tokenSource),
		userID:      "me",
		concurrency: 10,
		logger:      slog.Default(),
	}

	// Apply options
	for _, opt := range opts {
		opt(c)
	}

	// Default rate limiter if not set
	if c.rateLimiter == nil {
		c.rateLimiter = NewRateLimiter(5.0)
	}

	return c
}

// Close releases resources held by the client.
func (c *Client) Close() error {
	// HTTP client doesn't need explicit closing
	return nil
}

// request makes an HTTP request with rate limiting and retry logic.
// bodyBytes can be nil for requests without a body.
func (c *Client) request(ctx context.Context, op Operation, method, path string, bodyBytes []byte) ([]byte, error) {
	// Acquire rate limit tokens
	if err := c.rateLimiter.Acquire(ctx, op); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}

	reqURL := baseURL + path

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.calculateBackoff(attempt)
			c.logger.Debug("retrying request", "attempt", attempt, "backoff", backoff, "path", path)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		// Create a new reader for each attempt to ensure body can be re-read on retry
		var body io.Reader
		if bodyBytes != nil {
			body = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			continue // Retry on network errors
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response: %w", err)
			continue
		}

		// Check for success
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}

		// Handle specific error codes
		switch resp.StatusCode {
		case 429: // Rate limited
			// Log at Debug level since rate limiting is expected during high-volume syncs
			// and the retry logic handles it automatically
			c.logger.Debug("rate limited, backing off 30s", "path", path, "attempt", attempt)
			// Throttle the rate limiter to back off
			c.rateLimiter.Throttle(30 * time.Second)
			lastErr = fmt.Errorf("rate limited (429)")
			continue

		case 403: // Could be rate limit or permission error
			// Gmail returns 403 for quota exceeded with "rateLimitExceeded" reason
			if isRateLimitError(respBody) {
				// Log at Debug level since quota throttling is expected during high-volume syncs
				// and the retry logic handles it automatically
				c.logger.Debug("quota exceeded, backing off 60s", "path", path, "attempt", attempt)
				// Throttle the rate limiter - quota errors need longer backoff
				c.rateLimiter.Throttle(60 * time.Second)
				lastErr = fmt.Errorf("quota exceeded (403)")
				continue // Retry with backoff
			}
			// Actual permission error - don't retry
			return nil, fmt.Errorf("forbidden (403): %s", string(respBody))

		case 500, 502, 503, 504: // Server errors
			lastErr = fmt.Errorf("server error (%d)", resp.StatusCode)
			continue

		case 401: // Unauthorized - token might be expired
			// oauth2.Client should auto-refresh, but if it fails, don't retry
			return nil, fmt.Errorf("unauthorized (401): token may be invalid")

		case 404: // Not found
			return nil, &NotFoundError{Path: path}

		default: // Other client errors - don't retry
			return nil, fmt.Errorf("request failed (%d): %s", resp.StatusCode, string(respBody))
		}
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// calculateBackoff returns the backoff duration for a retry attempt.
// Uses exponential backoff with full jitter.
func (c *Client) calculateBackoff(attempt int) time.Duration {
	// Exponential: 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 600, 600...
	base := float64(uint(1) << uint(attempt))
	if base > maxBackoff {
		base = maxBackoff
	}

	// Full jitter: random value between 0 and base
	jittered := rand.Float64() * base
	return time.Duration(jittered * float64(time.Second))
}

// NotFoundError indicates a 404 response.
type NotFoundError struct {
	Path string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("not found: %s", e.Path)
}

// Gmail API JSON response types (unexported, used only for JSON unmarshaling).

type profileResponse struct {
	EmailAddress  string `json:"emailAddress"`
	MessagesTotal int64  `json:"messagesTotal"`
	ThreadsTotal  int64  `json:"threadsTotal"`
	HistoryID     string `json:"historyId"`
}

type gmailLabel struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Type                  string `json:"type"`
	MessagesTotal         int64  `json:"messagesTotal"`
	MessagesUnread        int64  `json:"messagesUnread"`
	MessageListVisibility string `json:"messageListVisibility"`
	LabelListVisibility   string `json:"labelListVisibility"`
}

type listLabelsResponse struct {
	Labels []gmailLabel `json:"labels"`
}

type gmailMessageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type listMessagesResponse struct {
	Messages           []gmailMessageRef `json:"messages"`
	NextPageToken      string            `json:"nextPageToken"`
	ResultSizeEstimate int64             `json:"resultSizeEstimate"`
}

type rawMessageResponse struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	HistoryID    string   `json:"historyId"`
	InternalDate string   `json:"internalDate"`
	SizeEstimate int64    `json:"sizeEstimate"`
	Raw          string   `json:"raw"` // base64url encoded (unpadded)
}

// decodeBase64URL decodes a base64url-encoded string, tolerating optional padding.
// Gmail typically returns unpadded base64url, but this function handles both cases.
// If padding is present, it validates that padding is correct (rejects malformed padding).
func decodeBase64URL(s string) ([]byte, error) {
	if strings.ContainsRune(s, '=') {
		// Input has padding - use URLEncoding which validates padding correctness
		return base64.URLEncoding.DecodeString(s)
	}
	// No padding - use RawURLEncoding for unpadded base64url
	return base64.RawURLEncoding.DecodeString(s)
}

type historyMessageChange struct {
	Message gmailMessageRef `json:"message"`
}

type historyLabelChangeJSON struct {
	Message  gmailMessageRef `json:"message"`
	LabelIDs []string        `json:"labelIds"`
}

type historyEntry struct {
	ID              string                   `json:"id"`
	MessagesAdded   []historyMessageChange   `json:"messagesAdded"`
	MessagesDeleted []historyMessageChange   `json:"messagesDeleted"`
	LabelsAdded     []historyLabelChangeJSON `json:"labelsAdded"`
	LabelsRemoved   []historyLabelChangeJSON `json:"labelsRemoved"`
}

type listHistoryResponse struct {
	History       []historyEntry `json:"history"`
	NextPageToken string         `json:"nextPageToken"`
	HistoryID     string         `json:"historyId"`
}

// GetProfile returns the authenticated user's profile.
func (c *Client) GetProfile(ctx context.Context) (*Profile, error) {
	path := fmt.Sprintf("/users/%s/profile", c.userID)
	data, err := c.request(ctx, OpProfile, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp profileResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}

	historyID, _ := strconv.ParseUint(resp.HistoryID, 10, 64)

	return &Profile{
		EmailAddress:  resp.EmailAddress,
		MessagesTotal: resp.MessagesTotal,
		ThreadsTotal:  resp.ThreadsTotal,
		HistoryID:     historyID,
	}, nil
}

// ListLabels returns all labels for the account.
func (c *Client) ListLabels(ctx context.Context) ([]*Label, error) {
	path := fmt.Sprintf("/users/%s/labels", c.userID)
	data, err := c.request(ctx, OpLabelsList, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp listLabelsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse labels: %w", err)
	}

	labels := make([]*Label, len(resp.Labels))
	for i, l := range resp.Labels {
		labels[i] = &Label{
			ID:                    l.ID,
			Name:                  l.Name,
			Type:                  l.Type,
			MessagesTotal:         l.MessagesTotal,
			MessagesUnread:        l.MessagesUnread,
			MessageListVisibility: l.MessageListVisibility,
			LabelListVisibility:   l.LabelListVisibility,
		}
	}
	return labels, nil
}

// ListMessages returns message IDs matching the query.
func (c *Client) ListMessages(ctx context.Context, query string, pageToken string) (*MessageListResponse, error) {
	params := url.Values{}
	params.Set("maxResults", "500")
	if query != "" {
		params.Set("q", query)
	}
	if pageToken != "" {
		params.Set("pageToken", pageToken)
	}

	path := fmt.Sprintf("/users/%s/messages?%s", c.userID, params.Encode())
	data, err := c.request(ctx, OpMessagesList, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp listMessagesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse messages: %w", err)
	}

	messages := make([]MessageID, len(resp.Messages))
	for i, m := range resp.Messages {
		messages[i] = MessageID(m)
	}

	return &MessageListResponse{
		Messages:           messages,
		NextPageToken:      resp.NextPageToken,
		ResultSizeEstimate: resp.ResultSizeEstimate,
	}, nil
}

// GetMessageRaw fetches a single message with raw MIME data.
func (c *Client) GetMessageRaw(ctx context.Context, messageID string) (*RawMessage, error) {
	path := fmt.Sprintf("/users/%s/messages/%s?format=raw", c.userID, messageID)
	data, err := c.request(ctx, OpMessagesGetRaw, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp rawMessageResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}

	// Decode raw MIME from base64url
	rawBytes, err := decodeBase64URL(resp.Raw)
	if err != nil {
		return nil, fmt.Errorf("decode raw MIME: %w", err)
	}

	historyID, _ := strconv.ParseUint(resp.HistoryID, 10, 64)
	internalDate, _ := strconv.ParseInt(resp.InternalDate, 10, 64)

	return &RawMessage{
		ID:           resp.ID,
		ThreadID:     resp.ThreadID,
		LabelIDs:     resp.LabelIDs,
		Snippet:      resp.Snippet,
		HistoryID:    historyID,
		InternalDate: internalDate,
		SizeEstimate: resp.SizeEstimate,
		Raw:          rawBytes,
	}, nil
}

// isRateLimitError checks if a 403 response is actually a rate limit error.
// Gmail returns 403 with "rateLimitExceeded" for quota exceeded instead of 429.
func isRateLimitError(body []byte) bool {
	// Check for common rate limit indicators in the response
	return bytes.Contains(body, []byte("rateLimitExceeded")) ||
		bytes.Contains(body, []byte("RATE_LIMIT_EXCEEDED")) ||
		bytes.Contains(body, []byte("Quota exceeded")) ||
		// Also check for userRateLimitExceeded which is another variant
		bytes.Contains(body, []byte("userRateLimitExceeded"))
}

// GetMessagesRawBatch fetches multiple messages in parallel with rate limiting.
func (c *Client) GetMessagesRawBatch(ctx context.Context, messageIDs []string) ([]*RawMessage, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	results := make([]*RawMessage, len(messageIDs))
	sem := make(chan struct{}, c.concurrency)

	g, ctx := errgroup.WithContext(ctx)

	for i, id := range messageIDs {
		i, id := i, id // Capture for goroutine

		g.Go(func() error {
			// Acquire semaphore
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return ctx.Err()
			}

			msg, err := c.GetMessageRaw(ctx, id)
			if err != nil {
				// Log but don't fail the batch - allow partial results.
				// 404s are expected (message deleted between history scan and fetch),
				// so log at debug level to avoid noise during incremental sync.
				var nfe *NotFoundError
				if errors.As(err, &nfe) {
					c.logger.Debug("message deleted before fetch", "id", id)
				} else {
					c.logger.Warn("failed to fetch message", "id", id, "error", err)
				}
				return nil
			}

			results[i] = msg
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return results, nil
}

// ListHistory returns changes since the given history ID.
func (c *Client) ListHistory(ctx context.Context, startHistoryID uint64, pageToken string) (*HistoryResponse, error) {
	params := url.Values{}
	params.Set("startHistoryId", strconv.FormatUint(startHistoryID, 10))
	params.Set("maxResults", "500")
	for _, ht := range []string{"messageAdded", "messageDeleted", "labelAdded", "labelRemoved"} {
		params.Add("historyTypes", ht)
	}
	if pageToken != "" {
		params.Set("pageToken", pageToken)
	}

	path := fmt.Sprintf("/users/%s/history?%s", c.userID, params.Encode())
	data, err := c.request(ctx, OpHistoryList, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp listHistoryResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse history: %w", err)
	}

	historyID, _ := strconv.ParseUint(resp.HistoryID, 10, 64)

	return &HistoryResponse{
		History:       mapHistoryEntries(resp.History),
		NextPageToken: resp.NextPageToken,
		HistoryID:     historyID,
	}, nil
}

// mapHistoryEntries converts JSON history entries to domain types.
func mapHistoryEntries(entries []historyEntry) []HistoryRecord {
	records := make([]HistoryRecord, len(entries))
	for i, h := range entries {
		id, _ := strconv.ParseUint(h.ID, 10, 64)
		records[i] = HistoryRecord{
			ID:              id,
			MessagesAdded:   mapMessageChanges(h.MessagesAdded),
			MessagesDeleted: mapMessageChanges(h.MessagesDeleted),
			LabelsAdded:     mapLabelChanges(h.LabelsAdded),
			LabelsRemoved:   mapLabelChanges(h.LabelsRemoved),
		}
	}
	return records
}

func mapMessageChanges(changes []historyMessageChange) []HistoryMessage {
	out := make([]HistoryMessage, len(changes))
	for i, c := range changes {
		out[i] = HistoryMessage{
			Message: MessageID(c.Message),
		}
	}
	return out
}

func mapLabelChanges(changes []historyLabelChangeJSON) []HistoryLabelChange {
	out := make([]HistoryLabelChange, len(changes))
	for i, c := range changes {
		out[i] = HistoryLabelChange{
			Message:  MessageID(c.Message),
			LabelIDs: c.LabelIDs,
		}
	}
	return out
}

// TrashMessage moves a message to trash.
func (c *Client) TrashMessage(ctx context.Context, messageID string) error {
	path := fmt.Sprintf("/users/%s/messages/%s/trash", c.userID, messageID)
	_, err := c.request(ctx, OpMessagesTrash, "POST", path, nil)
	return err
}

// DeleteMessage permanently deletes a message.
func (c *Client) DeleteMessage(ctx context.Context, messageID string) error {
	path := fmt.Sprintf("/users/%s/messages/%s", c.userID, messageID)
	_, err := c.request(ctx, OpMessagesDelete, "DELETE", path, nil)
	return err
}

// BatchDeleteMessages permanently deletes multiple messages.
func (c *Client) BatchDeleteMessages(ctx context.Context, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	if len(messageIDs) > 1000 {
		return fmt.Errorf("batch delete limited to 1000 messages, got %d", len(messageIDs))
	}

	body := struct {
		IDs []string `json:"ids"`
	}{IDs: messageIDs}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	path := fmt.Sprintf("/users/%s/messages/batchDelete", c.userID)
	_, err = c.request(ctx, OpMessagesBatchDelete, "POST", path, bodyBytes)
	return err
}

// --- Draft operations ---

// Gmail API JSON types for drafts.

type gmailDraftMessage struct {
	ID           string        `json:"id"`
	ThreadID     string        `json:"threadId"`
	LabelIDs     []string      `json:"labelIds"`
	Snippet      string        `json:"snippet"`
	InternalDate string        `json:"internalDate"`
	Payload      *gmailPayload `json:"payload"`
}

type gmailPayload struct {
	Headers []gmailHeader  `json:"headers"`
	Parts   []gmailPayload `json:"parts"`
	Body    *gmailBody     `json:"body"`
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailBody struct {
	Size int    `json:"size"`
	Data string `json:"data"` // base64url encoded
}

type gmailDraft struct {
	ID      string            `json:"id"`
	Message gmailDraftMessage `json:"message"`
}

type listDraftsResponse struct {
	Drafts        []gmailDraft `json:"drafts"`
	NextPageToken string       `json:"nextPageToken"`
}

// parseDraft converts a Gmail API draft response to our domain type.
func parseDraft(d gmailDraft) *Draft {
	msg := d.Message
	dm := DraftMessage{
		ID:       msg.ID,
		ThreadID: msg.ThreadID,
		LabelIDs: msg.LabelIDs,
		Snippet:  msg.Snippet,
	}

	if msg.InternalDate != "" {
		dm.Date, _ = strconv.ParseInt(msg.InternalDate, 10, 64)
	}

	if msg.Payload != nil {
		for _, h := range msg.Payload.Headers {
			switch strings.ToLower(h.Name) {
			case "from":
				dm.From = h.Value
			case "to":
				dm.To = splitAddresses(h.Value)
			case "cc":
				dm.Cc = splitAddresses(h.Value)
			case "bcc":
				dm.Bcc = splitAddresses(h.Value)
			case "subject":
				dm.Subject = h.Value
			}
		}

		dm.Body = extractBody(msg.Payload)
	}

	return &Draft{ID: d.ID, Message: dm}
}

// splitAddresses splits a comma-separated list of email addresses.
func splitAddresses(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// extractBody extracts the text/plain body from a message payload.
// Falls back to text/html if no plain text part exists.
func extractBody(p *gmailPayload) string {
	if p == nil {
		return ""
	}

	// Check this part directly
	if p.Body != nil && p.Body.Data != "" {
		// Determine content type from headers if available
		contentType := ""
		for _, h := range p.Headers {
			if strings.ToLower(h.Name) == "content-type" {
				contentType = strings.ToLower(h.Value)
				break
			}
		}
		if strings.Contains(contentType, "text/plain") || contentType == "" {
			decoded, err := decodeBase64URL(p.Body.Data)
			if err == nil {
				return string(decoded)
			}
		}
	}

	// Search parts recursively — prefer text/plain
	var htmlFallback string
	for i := range p.Parts {
		part := &p.Parts[i]
		contentType := ""
		for _, h := range part.Headers {
			if strings.ToLower(h.Name) == "content-type" {
				contentType = strings.ToLower(h.Value)
				break
			}
		}
		if part.Body != nil && part.Body.Data != "" {
			decoded, err := decodeBase64URL(part.Body.Data)
			if err == nil {
				if strings.Contains(contentType, "text/plain") {
					return string(decoded)
				}
				if strings.Contains(contentType, "text/html") && htmlFallback == "" {
					htmlFallback = string(decoded)
				}
			}
		}
		// Recurse into nested parts
		if len(part.Parts) > 0 {
			if body := extractBody(part); body != "" {
				return body
			}
		}
	}

	return htmlFallback
}

// buildRFC822Message builds a minimal RFC 822 message from DraftCompose fields.
func buildRFC822Message(d *DraftCompose) []byte {
	var buf bytes.Buffer
	if len(d.To) > 0 {
		fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(d.To, ", "))
	}
	if len(d.Cc) > 0 {
		fmt.Fprintf(&buf, "Cc: %s\r\n", strings.Join(d.Cc, ", "))
	}
	if len(d.Bcc) > 0 {
		fmt.Fprintf(&buf, "Bcc: %s\r\n", strings.Join(d.Bcc, ", "))
	}
	if d.Subject != "" {
		fmt.Fprintf(&buf, "Subject: %s\r\n", mimeEncodeHeader(d.Subject))
	}
	buf.WriteString("MIME-Version: 1.0\r\n")

	ct := d.ContentType
	if ct == "" {
		ct = "text/plain"
	}
	if ct == "text/plain" {
		fmt.Fprintf(&buf, "Content-Type: %s; charset=utf-8; format=flowed\r\n", ct)
	} else {
		fmt.Fprintf(&buf, "Content-Type: %s; charset=utf-8\r\n", ct)
	}
	buf.WriteString("Content-Transfer-Encoding: base64\r\n")
	buf.WriteString("\r\n")

	// Base64-encode the body to safely transport any UTF-8 content.
	encoded := base64.StdEncoding.EncodeToString([]byte(d.Body))
	// Wrap at 76 chars per RFC 2045.
	for len(encoded) > 76 {
		buf.WriteString(encoded[:76])
		buf.WriteString("\r\n")
		encoded = encoded[76:]
	}
	if len(encoded) > 0 {
		buf.WriteString(encoded)
		buf.WriteString("\r\n")
	}

	return buf.Bytes()
}

// mimeEncodeHeader encodes a header value using RFC 2047 encoded-word
// if it contains non-ASCII characters. ASCII-only values are returned as-is.
func mimeEncodeHeader(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

// ListDrafts returns drafts matching the optional query.
func (c *Client) ListDrafts(ctx context.Context, query string, maxResults int) ([]*Draft, error) {
	params := url.Values{}
	if maxResults > 0 {
		params.Set("maxResults", strconv.Itoa(maxResults))
	} else {
		params.Set("maxResults", "20")
	}
	if query != "" {
		params.Set("q", query)
	}
	// Request full message metadata so we can parse headers
	params.Set("includeSpamTrash", "false")

	path := fmt.Sprintf("/users/%s/drafts?%s", c.userID, params.Encode())
	data, err := c.request(ctx, OpDraftsList, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp listDraftsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse drafts list: %w", err)
	}

	// The list endpoint returns minimal data. Fetch each draft for full content.
	drafts := make([]*Draft, 0, len(resp.Drafts))
	for _, d := range resp.Drafts {
		full, err := c.GetDraft(ctx, d.ID)
		if err != nil {
			// Skip drafts that fail to fetch (may have been deleted)
			var nfe *NotFoundError
			if errors.As(err, &nfe) {
				continue
			}
			return nil, fmt.Errorf("get draft %s: %w", d.ID, err)
		}
		drafts = append(drafts, full)
	}

	return drafts, nil
}

// GetDraft returns a single draft by ID with full message content.
func (c *Client) GetDraft(ctx context.Context, draftID string) (*Draft, error) {
	params := url.Values{}
	params.Set("format", "full")

	path := fmt.Sprintf("/users/%s/drafts/%s?%s", c.userID, draftID, params.Encode())
	data, err := c.request(ctx, OpDraftsGet, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var d gmailDraft
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse draft: %w", err)
	}

	return parseDraft(d), nil
}

// CreateDraft creates a new draft.
func (c *Client) CreateDraft(ctx context.Context, compose *DraftCompose) (*Draft, error) {
	raw := buildRFC822Message(compose)
	encoded := base64.URLEncoding.EncodeToString(raw)

	body := struct {
		Message struct {
			Raw      string `json:"raw"`
			ThreadID string `json:"threadId,omitempty"`
		} `json:"message"`
	}{}
	body.Message.Raw = encoded
	if compose.ThreadID != "" {
		body.Message.ThreadID = compose.ThreadID
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal draft: %w", err)
	}

	path := fmt.Sprintf("/users/%s/drafts", c.userID)
	data, err := c.request(ctx, OpDraftsCreate, "POST", path, bodyBytes)
	if err != nil {
		return nil, err
	}

	var d gmailDraft
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse created draft: %w", err)
	}

	return parseDraft(d), nil
}

// UpdateDraft replaces the content of an existing draft.
func (c *Client) UpdateDraft(ctx context.Context, draftID string, compose *DraftCompose) (*Draft, error) {
	raw := buildRFC822Message(compose)
	encoded := base64.URLEncoding.EncodeToString(raw)

	body := struct {
		Message struct {
			Raw      string `json:"raw"`
			ThreadID string `json:"threadId,omitempty"`
		} `json:"message"`
	}{}
	body.Message.Raw = encoded
	if compose.ThreadID != "" {
		body.Message.ThreadID = compose.ThreadID
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal draft: %w", err)
	}

	path := fmt.Sprintf("/users/%s/drafts/%s", c.userID, draftID)
	data, err := c.request(ctx, OpDraftsUpdate, "PUT", path, bodyBytes)
	if err != nil {
		return nil, err
	}

	var d gmailDraft
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse updated draft: %w", err)
	}

	return parseDraft(d), nil
}

// DeleteDraft permanently deletes a draft.
func (c *Client) DeleteDraft(ctx context.Context, draftID string) error {
	path := fmt.Sprintf("/users/%s/drafts/%s", c.userID, draftID)
	_, err := c.request(ctx, OpDraftsDelete, "DELETE", path, nil)
	return err
}

// SendDraft sends a draft. The draft is removed from drafts afterward.
func (c *Client) SendDraft(ctx context.Context, draftID string) (*SentMessage, error) {
	body := struct {
		ID string `json:"id"`
	}{ID: draftID}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal send request: %w", err)
	}

	path := fmt.Sprintf("/users/%s/drafts/send", c.userID)
	data, err := c.request(ctx, OpDraftsSend, "POST", path, bodyBytes)
	if err != nil {
		return nil, err
	}

	var resp struct {
		ID       string   `json:"id"`
		ThreadID string   `json:"threadId"`
		LabelIDs []string `json:"labelIds"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse send response: %w", err)
	}

	return &SentMessage{
		ID:       resp.ID,
		ThreadID: resp.ThreadID,
		LabelIDs: resp.LabelIDs,
	}, nil
}

// --- Label operations ---

// ModifyMessageLabels adds and/or removes labels on a single message.
func (c *Client) ModifyMessageLabels(ctx context.Context, messageID string, addLabelIDs, removeLabelIDs []string) error {
	body := struct {
		AddLabelIDs    []string `json:"addLabelIds,omitempty"`
		RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
	}{
		AddLabelIDs:    addLabelIDs,
		RemoveLabelIDs: removeLabelIDs,
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal modify: %w", err)
	}

	path := fmt.Sprintf("/users/%s/messages/%s/modify", c.userID, messageID)
	_, err = c.request(ctx, OpModifyLabels, "POST", path, bodyBytes)
	return err
}

// BatchModifyLabels adds and/or removes labels on multiple messages (max 1000).
func (c *Client) BatchModifyLabels(ctx context.Context, messageIDs, addLabelIDs, removeLabelIDs []string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	if len(messageIDs) > 1000 {
		return fmt.Errorf("batch modify limited to 1000 messages, got %d", len(messageIDs))
	}

	body := struct {
		IDs            []string `json:"ids"`
		AddLabelIDs    []string `json:"addLabelIds,omitempty"`
		RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
	}{
		IDs:            messageIDs,
		AddLabelIDs:    addLabelIDs,
		RemoveLabelIDs: removeLabelIDs,
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal batch modify: %w", err)
	}

	path := fmt.Sprintf("/users/%s/messages/batchModify", c.userID)
	_, err = c.request(ctx, OpBatchModifyLabels, "POST", path, bodyBytes)
	return err
}

// CreateLabel creates a new user label.
func (c *Client) CreateLabel(ctx context.Context, name string) (*Label, error) {
	body := struct {
		Name                  string `json:"name"`
		MessageListVisibility string `json:"messageListVisibility"`
		LabelListVisibility   string `json:"labelListVisibility"`
	}{
		Name:                  name,
		MessageListVisibility: "show",
		LabelListVisibility:   "labelShow",
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal label: %w", err)
	}

	path := fmt.Sprintf("/users/%s/labels", c.userID)
	data, err := c.request(ctx, OpCreateLabel, "POST", path, bodyBytes)
	if err != nil {
		return nil, err
	}

	var resp gmailLabel
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse label: %w", err)
	}

	return &Label{
		ID:                    resp.ID,
		Name:                  resp.Name,
		Type:                  resp.Type,
		MessagesTotal:         resp.MessagesTotal,
		MessagesUnread:        resp.MessagesUnread,
		MessageListVisibility: resp.MessageListVisibility,
		LabelListVisibility:   resp.LabelListVisibility,
	}, nil
}

// DeleteLabel permanently deletes a user label by ID.
// Messages with this label are not deleted; the label is simply removed from them.
func (c *Client) DeleteLabel(ctx context.Context, labelID string) error {
	path := fmt.Sprintf("/users/%s/labels/%s", c.userID, labelID)
	_, err := c.request(ctx, OpDeleteLabel, "DELETE", path, nil)
	return err
}

// Ensure Client implements API interface.
var _ API = (*Client)(nil)

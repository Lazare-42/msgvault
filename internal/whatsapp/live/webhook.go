package live

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// InboundEvent is the payload emitted to the configured webhook after a
// WhatsApp message (inbound or outbound echo) has been archived.
type InboundEvent struct {
	Account         string    `json:"account"`
	Source          string    `json:"source"`
	ChatJID         string    `json:"chat_jid"`
	SenderJID       string    `json:"sender_jid,omitempty"`
	PushName        string    `json:"push_name,omitempty"`
	MessageID       string    `json:"message_id"`
	SourceMessageID string    `json:"source_message_id"`
	StoreMessageID  int64     `json:"store_message_id"`
	Body            string    `json:"body,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	IsFromMe        bool      `json:"is_from_me"`
	IsGroup         bool      `json:"is_group"`
	// HasAttachment, AttachmentMediaType, and AttachmentFilename describe a
	// media payload (image/video/document/audio/sticker) that was
	// successfully downloaded and durably stored for this message — a
	// message with a captionless attachment archives with an empty Body, so
	// without these a webhook consumer would see what looks like an empty
	// event even though real media was archived. A message whose attachment
	// failed to download still archives (see storeInboundAttachment) but is
	// not reported here as having one: a consumer that fetches media for
	// every HasAttachment=true event must not be pointed at bytes that were
	// never actually stored.
	HasAttachment       bool   `json:"has_attachment"`
	AttachmentMediaType string `json:"attachment_media_type,omitempty"`
	AttachmentFilename  string `json:"attachment_filename,omitempty"`
}

// WebhookNotifier delivers InboundEvents to a local HTTP consumer with an
// HMAC-SHA256 signature. Delivery is asynchronous and best-effort: events
// are queued on a bounded channel and retried a few times; consumers must
// treat the source_message_id as an idempotency key and rely on backfill
// for anything dropped.
type WebhookNotifier struct {
	url    string
	secret []byte
	client *http.Client
	logger *slog.Logger

	queue  chan InboundEvent
	done   chan struct{}
	closed sync.Once
}

type WebhookOptions struct {
	URL    string
	Secret string
	Logger *slog.Logger
}

func NewWebhookNotifier(opts WebhookOptions) *WebhookNotifier {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	n := &WebhookNotifier{
		url:    opts.URL,
		secret: []byte(opts.Secret),
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
		queue:  make(chan InboundEvent, 256),
		done:   make(chan struct{}),
	}
	go n.run()
	return n
}

// Notify enqueues an event. Never blocks; drops (with a log line) when the
// queue is full.
func (n *WebhookNotifier) Notify(_ context.Context, event InboundEvent) {
	select {
	case n.queue <- event:
	default:
		n.logger.Warn("whatsapp webhook queue full; dropping event",
			"source_message_id", event.SourceMessageID)
	}
}

func (n *WebhookNotifier) Close() {
	n.closed.Do(func() { close(n.done) })
}

func (n *WebhookNotifier) run() {
	backoffs := []time.Duration{time.Second, 5 * time.Second, 25 * time.Second}
	for {
		select {
		case <-n.done:
			return
		case event := <-n.queue:
			var lastErr error
			for attempt := 0; attempt <= len(backoffs); attempt++ {
				if attempt > 0 {
					select {
					case <-n.done:
						return
					case <-time.After(backoffs[attempt-1]):
					}
				}
				if lastErr = n.deliver(event); lastErr == nil {
					break
				}
			}
			if lastErr != nil {
				n.logger.Warn("whatsapp webhook delivery failed; giving up",
					"source_message_id", event.SourceMessageID, "err", lastErr)
			}
		}
	}
}

func (n *WebhookNotifier) deliver(event InboundEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Msgvault-Event", "whatsapp.message")
	req.Header.Set("X-Msgvault-Idempotency-Key", event.SourceMessageID)
	if len(n.secret) > 0 {
		mac := hmac.New(sha256.New, n.secret)
		mac.Write(body)
		req.Header.Set("X-Msgvault-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("webhook returned %s", resp.Status)
}

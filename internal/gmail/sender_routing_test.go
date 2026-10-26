package gmail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDraftRoutingHeaders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, multipart := range []bool{false, true} {
		d := &DraftCompose{From: `Reports <reports@example.com>`, ReplyTo: []string{`"Owner, Example" <owner@example.com>, Backup <backup@example.com>`}, Body: "<b>hello</b>", ContentType: "text/html", InReplyTo: "<parent@example.com>"}
		if multipart {
			d.Attachments = []DraftAttachment{{Filename: "report.txt", Content: []byte("attachment")}}
		}
		raw, err := BuildDraftMIME(d)
		require.NoError(err)
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		require.NoError(err)
		replies, err := msg.Header.AddressList("Reply-To")
		require.NoError(err)
		require.Len(replies, 2)
		assert.Equal("Owner, Example", replies[0].Name)
		assert.Equal("owner@example.com", replies[0].Address)
		assert.Equal("<parent@example.com>", msg.Header.Get("In-Reply-To"))
	}
}

func TestDraftRoutingRejectsInvalidHeaders(t *testing.T) {
	require := require.New(t)
	for _, value := range []string{"not an address", "a@example.com\r\nBcc: b@example.com", "a@example.com\x00", "=?utf-8?Q?bad=0Aname?= <a@example.com>"} {
		_, err := BuildDraftMIME(&DraftCompose{From: value})
		require.Error(err)
		_, err = BuildDraftMIME(&DraftCompose{ReplyTo: []string{value}})
		require.Error(err)
	}
	_, err := BuildDraftMIME(&DraftCompose{From: "a@example.com, b@example.com"})
	require.Error(err)
}

func TestGmailDraftRoutingRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var stored []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		var err error
		stored, err = base64.URLEncoding.DecodeString(body.Message.Raw)
		if !assert.NoError(err) {
			http.Error(w, "invalid base64", http.StatusBadRequest)
			return
		}
		message, err := mail.ReadMessage(bytes.NewReader(stored))
		if !assert.NoError(err) {
			http.Error(w, "invalid MIME", http.StatusBadRequest)
			return
		}
		headers := []map[string]string{}
		for _, name := range []string{"From", "Reply-To"} {
			headers = append(headers, map[string]string{"name": name, "value": message.Header.Get(name)})
		}
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{"id": "draft-1", "message": map[string]any{"id": "msg-1", "payload": map[string]any{"headers": headers}}}))
	}))
	defer server.Close()
	client := &Client{httpClient: &http.Client{Transport: &rewriteTransport{base: server.URL, wrapped: http.DefaultTransport}}, userID: "me", logger: slog.Default(), rateLimiter: NewRateLimiter(1000)}
	compose := &DraftCompose{From: "reports@example.com", ReplyTo: []string{`"Owner, Example" <owner@example.com>`}, Body: "body"}
	created, err := client.CreateDraft(t.Context(), compose)
	require.NoError(err)
	assert.Contains(created.Message.From, "reports@example.com")
	require.Len(created.Message.ReplyTo, 1)
	reply, err := mail.ParseAddress(created.Message.ReplyTo[0])
	require.NoError(err)
	assert.Equal("Owner, Example", reply.Name)
	updated, err := client.UpdateDraft(t.Context(), created.ID, compose)
	require.NoError(err)
	assert.Equal(created.Message.ReplyTo, updated.Message.ReplyTo)
}

func TestGmailSendUncertainDoesNotRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		{
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
				_, err := w.Write([]byte("invalid response"))
				assert.NoError(err)
			}))
			defer server.Close()
			client := &Client{httpClient: &http.Client{Transport: &rewriteTransport{base: server.URL, wrapped: http.DefaultTransport}}, userID: "me", logger: slog.Default(), rateLimiter: NewRateLimiter(1000)}
			_, err := client.SendDraft(t.Context(), "draft-1")
			var outcome *SendOutcomeError
			require.ErrorAs(err, &outcome)
			assert.Equal(status == http.StatusOK, outcome.Accepted)
			assert.Equal(int32(1), requests.Load())
		}
	}
}

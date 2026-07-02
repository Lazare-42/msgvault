package gmail

import (
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRFC822Message_NoAttachments_SinglePart(t *testing.T) {
	d := &DraftCompose{
		To:      []string{"a@example.com"},
		Subject: "hi",
		Body:    "hello world",
	}
	rawBytes, err := BuildDraftMIME(d)
	require.NoError(t, err)
	raw := string(rawBytes)
	assert.NotContains(t, raw, "multipart/mixed")
	assert.Contains(t, raw, "Content-Transfer-Encoding: base64")
}

func TestBuildRFC822Message_ReplyHeaders(t *testing.T) {
	t.Run("in_reply_to defaults references", func(t *testing.T) {
		d := &DraftCompose{
			To:        []string{"a@example.com"},
			Subject:   "RE: hi",
			Body:      "hello",
			InReplyTo: "<orig-123@mail.gmail.com>",
		}
		raw, err := BuildDraftMIME(d)
		require.NoError(t, err)
		msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
		require.NoError(t, err)
		assert.Equal(t, "<orig-123@mail.gmail.com>", msg.Header.Get("In-Reply-To"))
		// References defaults to In-Reply-To when not supplied.
		assert.Equal(t, "<orig-123@mail.gmail.com>", msg.Header.Get("References"))
	})

	t.Run("explicit references chain", func(t *testing.T) {
		d := &DraftCompose{
			To:         []string{"a@example.com"},
			Subject:    "RE: hi",
			Body:       "hello",
			InReplyTo:  "<msg-2@x>",
			References: "<msg-0@x> <msg-1@x> <msg-2@x>",
		}
		raw, err := BuildDraftMIME(d)
		require.NoError(t, err)
		msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
		require.NoError(t, err)
		assert.Equal(t, "<msg-0@x> <msg-1@x> <msg-2@x>", msg.Header.Get("References"))
	})

	t.Run("no reply headers when not a reply", func(t *testing.T) {
		d := &DraftCompose{To: []string{"a@example.com"}, Subject: "hi", Body: "hello"}
		raw, err := BuildDraftMIME(d)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "In-Reply-To:")
		assert.NotContains(t, string(raw), "References:")
	})
}

func TestBuildRFC822Message_WithAttachments_Multipart(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake attachment bytes \x00\x01\x02")
	d := &DraftCompose{
		To:      []string{"alice@example.com", "bob@example.com"},
		Subject: "Synthetic attachment draft",
		Body:    "Attached test content.",
		Attachments: []DraftAttachment{
			{Filename: "sample.pdf", ContentType: "application/pdf", Content: pdf},
		},
	}

	raw, err := BuildDraftMIME(d)
	require.NoError(t, err)
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	require.NoError(t, err)

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	assert.Equal(t, "multipart/mixed", mediaType)

	mr := multipart.NewReader(msg.Body, params["boundary"])

	// Part 1: body.
	body, err := mr.NextPart()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(body.Header.Get("Content-Type"), "text/plain"))

	// Part 2: attachment verifies content round-trips through base64.
	att, err := mr.NextPart()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(att.Header.Get("Content-Disposition"), "attachment"))
	encoded, err := io.ReadAll(att)
	require.NoError(t, err)
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(encoded), "\r\n", ""))
	require.NoError(t, err)
	assert.Equal(t, pdf, decoded)

	_, err = mr.NextPart()
	assert.ErrorIs(t, err, io.EOF)
}

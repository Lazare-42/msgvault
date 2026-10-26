package imap

import (
	"bytes"
	"context"
	"net/mail"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mailclient"
)

func TestSenderRoutingDraftLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := newDraftTestClient(t)
	client, err := mailclient.New(backend, testIMAPUsername, []string{"reports@example.com"})
	require.NoError(err)
	compose := &gmail.DraftCompose{
		From: `Reports <reports@example.com>`, ReplyTo: []string{`"Owner, Example" <owner@example.com>`},
		To: []string{"reader@example.com"}, Body: "<b>report</b>", ContentType: "text/html",
		Attachments: []gmail.DraftAttachment{{Filename: "report.txt", Content: []byte("report")}},
	}
	draft, err := client.CreateDraft(t.Context(), compose)
	require.NoError(err)
	read, err := client.GetDraft(t.Context(), draft.ID)
	require.NoError(err)
	require.Len(read.Message.ReplyTo, 1)
	address, err := mail.ParseAddress(read.Message.ReplyTo[0])
	require.NoError(err)
	assert.Equal("Owner, Example", address.Name)
	assert.Equal("owner@example.com", address.Address)
	// A content replacement must keep the sender and reply destination.
	compose.From, compose.ReplyTo = "", nil
	updated, err := client.UpdateDraft(t.Context(), draft.ID, compose)
	require.NoError(err)
	var submissions int
	backend.smtpSend = func(_ context.Context, from string, to []string, raw []byte) error {
		submissions++
		assert.Equal("reports@example.com", from)
		assert.Equal(testIMAPUsername, backend.config.Username, "authentication account must not change")
		assert.Equal([]string{"reader@example.com"}, to)
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		require.NoError(err)
		sender, err := msg.Header.AddressList("From")
		require.NoError(err)
		require.Len(sender, 1)
		assert.Equal("reports@example.com", sender[0].Address)
		replies, err := msg.Header.AddressList("Reply-To")
		require.NoError(err)
		require.Len(replies, 1)
		assert.Equal("owner@example.com", replies[0].Address)
		assert.Contains(msg.Header.Get("Content-Type"), "multipart/mixed")
		return nil
	}
	sent, err := client.SendDraft(t.Context(), updated.ID)
	require.NoError(err)
	assert.Equal(1, submissions)
	stored, err := client.GetDraft(t.Context(), sent.ID)
	require.NoError(err)
	assert.NotEmpty(stored.Message.ReplyTo)
	// A non-draft mailbox must never be submitted, even with a valid sender.
	_, err = client.SendDraft(t.Context(), sent.ID)
	require.Error(err)
	assert.Equal(1, submissions)
}

func TestSenderPolicyAndExplicitReplyToClear(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := newDraftTestClient(t)
	client, err := mailclient.New(backend, testIMAPUsername, nil)
	require.NoError(err)
	_, err = client.CreateDraft(t.Context(), &gmail.DraftCompose{From: "other@example.com", Body: "body"})
	require.ErrorContains(err, "not allowed")
	draft, err := client.CreateDraft(t.Context(), &gmail.DraftCompose{To: []string{"reader@example.com"}, ReplyTo: []string{"owner@example.com"}, Body: "body"})
	require.NoError(err)
	updated, err := client.UpdateDraft(t.Context(), draft.ID, &gmail.DraftCompose{To: []string{"reader@example.com"}, ReplyTo: []string{}, Body: "new"})
	require.NoError(err)
	read, err := client.GetDraft(t.Context(), updated.ID)
	require.NoError(err)
	assert.Empty(read.Message.ReplyTo)
	address, err := mail.ParseAddress(read.Message.From)
	require.NoError(err)
	assert.Equal(testIMAPUsername, address.Address)
	// An externally-created draft must not bypass the send-time allowlist.
	external, err := backend.CreateDraft(t.Context(), &gmail.DraftCompose{From: "other@example.com", To: []string{"reader@example.com"}, Body: "body"})
	require.NoError(err)
	submissions := 0
	backend.smtpSend = func(context.Context, string, []string, []byte) error { submissions++; return nil }
	_, err = client.SendDraft(t.Context(), external.ID)
	require.ErrorContains(err, "not allowed")
	assert.Zero(submissions)
}

func TestSendAcceptedButSentCopyFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := newDraftTestClient(t)
	draft, err := backend.CreateDraft(t.Context(), &gmail.DraftCompose{To: []string{"reader@example.com"}, Body: "body"})
	require.NoError(err)
	backend.smtpSend = func(context.Context, string, []string, []byte) error {
		// Break the archive connection only after SMTP accepts the message.
		backend.config.Host = "127.0.0.1"
		backend.config.Port = 0
		return backend.Close()
	}
	_, err = backend.SendDraft(t.Context(), draft.ID)
	require.Error(err)
	var outcome *gmail.SendOutcomeError
	require.ErrorAs(err, &outcome)
	assert.True(outcome.Accepted)
	assert.Contains(err.Error(), "before retrying")
}

func TestDraftRejectsMultipleSenders(t *testing.T) {
	require := require.New(t)
	for _, header := range []string{
		"From: a@example.com\r\nFrom: b@example.com\r\n",
		"From: a@example.com, b@example.com\r\n",
	} {
		_, err := draftFromRaw(&gmail.RawMessage{Raw: []byte(header + "To: reader@example.com\r\n\r\nbody")})
		require.Error(err)
	}
}

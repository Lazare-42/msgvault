package imap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
)

const (
	testIMAPUsername = "user@example.com"
	testIMAPPassword = "test-password"
)

func TestClientCreateDraftAppendsToDrafts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := newDraftTestClient(t)

	compose := &gmailapi.DraftCompose{
		To:        []string{"alice@example.com"},
		Cc:        []string{"bob@example.com"},
		Subject:   "Synthetic IMAP draft",
		Body:      "hello from draft",
		InReplyTo: "<orig@example.com>",
		ThreadID:  "INBOX|1",
	}

	draft, err := client.CreateDraft(context.Background(), compose)
	require.NoError(err)
	require.NotNil(draft)
	assert.Equal("Synthetic IMAP draft", draft.Message.Subject)
	assert.Equal([]string{"alice@example.com"}, draft.Message.To)

	mailbox, uid, err := parseCompositeID(draft.ID)
	require.NoError(err)
	assert.Equal("Drafts", mailbox)
	assert.NotZero(uid)

	raw, err := client.GetMessageRaw(context.Background(), draft.ID)
	require.NoError(err)
	require.NotNil(raw)

	msg, err := mail.ReadMessage(bytes.NewReader(raw.Raw))
	require.NoError(err)
	assert.Contains(msg.Header.Get("To"), "alice@example.com")
	assert.Contains(msg.Header.Get("Cc"), "bob@example.com")
	assert.Equal("Synthetic IMAP draft", msg.Header.Get("Subject"))
	assert.NotEmpty(msg.Header.Get("Message-ID"))
	assert.NotEqual("<orig@example.com>", msg.Header.Get("Message-ID"))
	assert.Equal("<orig@example.com>", msg.Header.Get("In-Reply-To"))
	assert.Equal("<orig@example.com>", msg.Header.Get("References"))
	assert.Contains(string(raw.Raw), "Content-Transfer-Encoding: base64")

	flags := fetchMessageFlags(t, client, mailbox, uid)
	assert.Contains(flags, goimap.FlagDraft)
}

func TestClientDraftCRUD(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := newDraftTestClient(t)

	first, err := client.CreateDraft(context.Background(), &gmailapi.DraftCompose{
		To:      []string{"alice@example.com"},
		Subject: "first draft",
		Body:    "first body",
	})
	require.NoError(err)

	got, err := client.GetDraft(context.Background(), first.ID)
	require.NoError(err)
	assert.Equal("first draft", got.Message.Subject)
	assert.Equal("first body", strings.TrimSpace(got.Message.Body))

	listed, err := client.ListDrafts(context.Background(), "first", 10)
	require.NoError(err)
	require.Len(listed, 1)
	assert.Equal(first.ID, listed[0].ID)

	updated, err := client.UpdateDraft(context.Background(), first.ID, &gmailapi.DraftCompose{
		To:      []string{"bob@example.com"},
		Subject: "updated draft",
		Body:    "updated body",
	})
	require.NoError(err)
	assert.NotEqual(first.ID, updated.ID)
	assert.Equal("updated draft", updated.Message.Subject)

	_, err = client.GetDraft(context.Background(), first.ID)
	require.Error(err)

	listed, err = client.ListDrafts(context.Background(), "", 10)
	require.NoError(err)
	require.Len(listed, 1)
	assert.Equal(updated.ID, listed[0].ID)
	require.NoError(client.DeleteDraft(context.Background(), updated.ID))
	listed, err = client.ListDrafts(context.Background(), "", 10)
	require.NoError(err)
	assert.Empty(listed)
}

func TestClientSendDraftSendsAppendsSentAndDeletesDraft(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := newDraftTestClient(t)

	var sentFrom string
	var sentTo []string
	var sentRaw []byte
	client.smtpSend = func(_ context.Context, from string, to []string, raw []byte) error {
		sentFrom = from
		sentTo = append([]string(nil), to...)
		sentRaw = append([]byte(nil), raw...)
		return nil
	}

	draft, err := client.CreateDraft(context.Background(), &gmailapi.DraftCompose{
		To:      []string{"alice@example.com"},
		Cc:      []string{"bob@example.com"},
		Bcc:     []string{"carol@example.com"},
		Subject: "send me",
		Body:    "send body",
	})
	require.NoError(err)

	sent, err := client.SendDraft(context.Background(), draft.ID)
	require.NoError(err)
	require.NotNil(sent)
	assert.Equal(testIMAPUsername, sentFrom)
	assert.ElementsMatch([]string{"alice@example.com", "bob@example.com", "carol@example.com"}, sentTo)
	assert.Contains(string(sentRaw), "Subject: send me")
	assert.Equal([]string{"Sent Items"}, sent.LabelIDs)

	_, err = client.GetDraft(context.Background(), draft.ID)
	require.Error(err)

	raw, err := client.GetMessageRaw(context.Background(), sent.ID)
	require.NoError(err)
	require.NotNil(raw)
	msg, err := mail.ReadMessage(bytes.NewReader(raw.Raw))
	require.NoError(err)
	assert.Equal("send me", msg.Header.Get("Subject"))

	drafts, err := client.ListDrafts(context.Background(), "", 10)
	require.NoError(err)
	assert.Empty(drafts)
}

func TestClientModifyMessageLabelsFlagSubset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := newDraftTestClient(t)
	id := appendTestMessage(t, client, "INBOX", &gmailapi.DraftCompose{
		To:      []string{"alice@example.com"},
		Subject: "label flags",
		Body:    "body",
	})
	_, uid, err := parseCompositeID(id)
	require.NoError(err)
	require.NoError(client.ModifyMessageLabels(
		context.Background(),
		id,
		[]string{"STARRED"},
		[]string{"UNREAD"},
	))
	flags := fetchMessageFlags(t, client, "INBOX", uid)
	assert.Contains(flags, goimap.FlagFlagged)
	assert.Contains(flags, goimap.FlagSeen)
	require.NoError(client.ModifyMessageLabels(
		context.Background(),
		id,
		[]string{"UNREAD"},
		[]string{"STARRED"},
	))
	flags = fetchMessageFlags(t, client, "INBOX", uid)
	assert.NotContains(flags, goimap.FlagFlagged)
	assert.NotContains(flags, goimap.FlagSeen)
}

func TestClientModifyMessageLabelsArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := newDraftTestClient(t)
	id := appendTestMessage(t, client, "INBOX", &gmailapi.DraftCompose{
		To:      []string{"alice@example.com"},
		Subject: "archive me",
		Body:    "body",
	})
	require.NoError(client.ModifyMessageLabels(
		context.Background(),
		id,
		nil,
		[]string{"INBOX"},
	))

	_, err := client.GetMessageRaw(context.Background(), id)
	require.Error(err)

	archiveIDs := listMailboxMessageIDs(t, client, "Archive")
	require.Len(archiveIDs, 1)
	raw, err := client.GetMessageRaw(context.Background(), archiveIDs[0])
	require.NoError(err)
	assert.Contains(string(raw.Raw), "Subject: archive me")
}

func TestClientModifyMessageLabelsRejectsArbitraryLabels(t *testing.T) {
	client := newDraftTestClient(t)
	id := appendTestMessage(t, client, "INBOX", &gmailapi.DraftCompose{
		To:      []string{"alice@example.com"},
		Subject: "label reject",
		Body:    "body",
	})

	err := client.ModifyMessageLabels(context.Background(), id, []string{"Projects"}, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "unsupported")
}

func newDraftTestClient(t *testing.T) *Client {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(testIMAPUsername, testIMAPPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archive", nil))
	require.NoError(t, user.Create("Drafts", nil))
	require.NoError(t, user.Create("Sent Items", nil))
	memServer.AddUser(user)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps: goimap.CapSet{
			goimap.CapIMAP4rev1:  {},
			goimap.CapUIDPlus:    {},
			goimap.CapSpecialUse: {},
		},
		InsecureAuth: true,
		Logger:       log.New(io.Discard, "", 0),
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := addr.Port
	client := NewClient(&Config{
		Host:     "127.0.0.1",
		Port:     port,
		Username: testIMAPUsername,
	}, testIMAPPassword)

	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		assert.NoError(t, server.Close())
		select {
		case err := <-errCh:
			assert.NoError(t, err)
		case <-time.After(time.Second):
			assert.Fail(t, "IMAP server did not stop")
		}
	})

	return client
}

func appendTestMessage(
	t *testing.T,
	client *Client,
	mailbox string,
	compose *gmailapi.DraftCompose,
) string {
	t.Helper()

	raw, err := gmailapi.BuildDraftMIME(compose)
	require.NoError(t, err)

	var id string
	err = client.withConn(context.Background(), func(conn *imapclient.Client) error {
		var err error
		id, err = client.appendMessageLocked(conn, mailbox, raw, nil)
		return err
	})
	require.NoError(t, err)
	return id
}

func listMailboxMessageIDs(t *testing.T, client *Client, mailbox string) []string {
	t.Helper()

	var ids []string
	err := client.withConn(context.Background(), func(conn *imapclient.Client) error {
		if err := client.selectMailbox(mailbox); err != nil {
			return err
		}
		searchData, err := conn.UIDSearch(&goimap.SearchCriteria{}, nil).Wait()
		if err != nil {
			return fmt.Errorf("read test mailbox: %w", err)
		}
		uidSet, ok := searchData.All.(goimap.UIDSet)
		if !ok {
			return nil
		}
		uids, _ := uidSet.Nums()
		for _, uid := range uids {
			ids = append(ids, compositeID(mailbox, uid))
		}
		return nil
	})
	require.NoError(t, err)
	return ids
}

func fetchMessageFlags(t *testing.T, client *Client, mailbox string, uid goimap.UID) []goimap.Flag {
	t.Helper()

	var flags []goimap.Flag
	err := client.withConn(context.Background(), func(conn *imapclient.Client) error {
		if err := client.selectMailbox(mailbox); err != nil {
			return err
		}
		var uidSet goimap.UIDSet
		uidSet.AddNum(uid)
		msgs, err := conn.Fetch(uidSet, &goimap.FetchOptions{
			UID:   true,
			Flags: true,
		}).Collect()
		if err != nil {
			return fmt.Errorf("read test mailbox: %w", err)
		}
		if len(msgs) != 1 {
			return fmt.Errorf("expected 1 fetched draft, got %d", len(msgs))
		}
		flags = msgs[0].Flags
		return nil
	})
	require.NoError(t, err)
	return flags
}

func TestClientCreateDraftRejectsThreadWithoutReplyHeader(t *testing.T) {
	for _, header := range []string{"", " \t"} {
		t.Run(fmt.Sprintf("header_%q", header), func(t *testing.T) {
			client := &Client{}
			draft, err := client.CreateDraft(t.Context(), &gmailapi.DraftCompose{ThreadID: "INBOX|1", InReplyTo: header})
			require.ErrorContains(t, err, "IMAP replies require in_reply_to")
			assert.Nil(t, draft)
		})
	}
}

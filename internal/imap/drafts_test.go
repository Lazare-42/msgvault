package imap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
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
	client := newDraftTestClient(t)

	compose := &gmailapi.DraftCompose{
		To:        []string{"alice@example.com"},
		Cc:        []string{"bob@example.com"},
		Subject:   "Synthetic IMAP draft",
		Body:      "hello from draft",
		InReplyTo: "<orig@example.com>",
	}

	draft, err := client.CreateDraft(context.Background(), compose)
	require.NoError(t, err)
	require.NotNil(t, draft)
	assert.Equal(t, "Synthetic IMAP draft", draft.Message.Subject)
	assert.Equal(t, []string{"alice@example.com"}, draft.Message.To)

	mailbox, uid, err := parseCompositeID(draft.ID)
	require.NoError(t, err)
	assert.Equal(t, "Drafts", mailbox)
	assert.NotZero(t, uid)

	raw, err := client.GetMessageRaw(context.Background(), draft.ID)
	require.NoError(t, err)
	require.NotNil(t, raw)

	msg, err := mail.ReadMessage(bytes.NewReader(raw.Raw))
	require.NoError(t, err)
	assert.Contains(t, msg.Header.Get("To"), "alice@example.com")
	assert.Contains(t, msg.Header.Get("Cc"), "bob@example.com")
	assert.Equal(t, "Synthetic IMAP draft", msg.Header.Get("Subject"))
	assert.Equal(t, "<orig@example.com>", msg.Header.Get("In-Reply-To"))
	assert.Equal(t, "<orig@example.com>", msg.Header.Get("References"))
	assert.Contains(t, string(raw.Raw), "Content-Transfer-Encoding: base64")

	flags := fetchDraftFlags(t, client, mailbox, uid)
	assert.Contains(t, flags, goimap.FlagDraft)
}

func newDraftTestClient(t *testing.T) *Client {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(testIMAPUsername, testIMAPPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Drafts", nil))
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

	port := listener.Addr().(*net.TCPAddr).Port
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

func fetchDraftFlags(t *testing.T, client *Client, mailbox string, uid goimap.UID) []goimap.Flag {
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
			return err
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

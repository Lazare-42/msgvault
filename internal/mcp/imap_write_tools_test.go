package mcp

import (
	"context"
	"io"
	"log"
	"net"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
)

const (
	testIMAPAccount  = "user@example.com"
	testIMAPPassword = "test-password"
)

// startIMAPTestServer runs an in-memory IMAP server and returns its port.
// Mirrors the server setup used by the internal/imap draft tests so the
// MCP handlers are exercised against a real IMAP implementation.
func startIMAPTestServer(t *testing.T) int {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(testIMAPAccount, testIMAPPassword)
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
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	return listener.Addr().(*net.TCPAddr).Port
}

// imapFactory returns a GmailClientFactory that builds a fresh IMAP client
// per call, matching production behavior where each tool invocation gets a
// new authenticated client that it closes when done.
func imapFactory(t *testing.T, port int) GmailClientFactory {
	t.Helper()
	return func(_ context.Context, email string) (gmail.API, error) {
		assert.Equal(t, testIMAPAccount, email, "factory should receive the resolved account email")
		return imaplib.NewClient(&imaplib.Config{
			Host:     "127.0.0.1",
			Port:     port,
			Username: testIMAPAccount,
		}, testIMAPPassword), nil
	}
}

// TestDraftAndLabelToolsOverIMAP drives the MCP draft/label write handlers
// through a GmailClientFactory that returns a real IMAP client (the same
// gmail.API implementation used for IMAP and Microsoft 365 sources),
// proving the write toolset works for non-Gmail accounts end to end.
func TestDraftAndLabelToolsOverIMAP(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	port := startIMAPTestServer(t)
	h := &handlers{gmailFactory: imapFactory(t, port)}
	account := map[string]any{"account": testIMAPAccount}

	// create_draft
	created := runTool[struct {
		DraftID string `json:"draft_id"`
		Subject string `json:"subject"`
	}](t, ToolCreateDraft, h.createDraft, map[string]any{
		"account": testIMAPAccount,
		"to":      "alice@example.com",
		"subject": "Quarterly report",
		"body":    "Please find the summary below.",
	})
	require.NotEmpty(created.DraftID, "create_draft should return a draft ID")
	assert.Equal("Quarterly report", created.Subject)

	// list_drafts sees the created draft
	drafts := runTool[[]struct {
		DraftID string   `json:"draft_id"`
		Subject string   `json:"subject"`
		To      []string `json:"to"`
	}](t, ToolListDrafts, h.listDrafts, account)
	require.Len(drafts, 1)
	assert.Equal(created.DraftID, drafts[0].DraftID)
	assert.Equal("Quarterly report", drafts[0].Subject)
	assert.Equal([]string{"alice@example.com"}, drafts[0].To)

	// get_draft returns the full draft
	full := runTool[struct {
		ID      string `json:"ID"`
		Message struct {
			Subject string `json:"Subject"`
			Body    string `json:"Body"`
		} `json:"Message"`
	}](t, ToolGetDraft, h.getDraft, map[string]any{
		"account":  testIMAPAccount,
		"draft_id": created.DraftID,
	})
	assert.Equal(created.DraftID, full.ID)
	assert.Equal("Quarterly report", full.Message.Subject)
	assert.Contains(full.Message.Body, "summary below")

	// update_draft replaces content
	updated := runTool[struct {
		DraftID string `json:"draft_id"`
		Status  string `json:"status"`
	}](t, ToolUpdateDraft, h.updateDraft, map[string]any{
		"account":  testIMAPAccount,
		"draft_id": created.DraftID,
		"to":       "alice@example.com",
		"subject":  "Quarterly report v2",
		"body":     "Updated body.",
	})
	assert.Equal("updated", updated.Status)
	require.NotEmpty(updated.DraftID)

	// create_label creates an IMAP mailbox
	label := runTool[struct {
		ID   string `json:"ID"`
		Name string `json:"Name"`
	}](t, ToolCreateLabel, h.createLabel, map[string]any{
		"account": testIMAPAccount,
		"name":    "Recruiting",
	})
	assert.Equal("Recruiting", label.ID)

	// list_gmail_labels includes system mailboxes and the new folder
	labels := runTool[[]struct {
		ID   string `json:"ID"`
		Name string `json:"Name"`
	}](t, ToolListGmailLabels, h.listGmailLabels, account)
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	assert.Contains(names, "INBOX")
	assert.Contains(names, "Recruiting")

	// modify_labels files the draft message into the new folder via a MOVE
	moved := runTool[struct {
		MessageCount int    `json:"message_count"`
		Status       string `json:"status"`
	}](t, ToolModifyLabels, h.modifyLabels, map[string]any{
		"account":     testIMAPAccount,
		"message_ids": updated.DraftID,
		"add_labels":  "folder:Recruiting",
	})
	assert.Equal(1, moved.MessageCount)
	assert.Equal("modified", moved.Status)

	// The draft left the Drafts mailbox, so list_drafts is empty again.
	remaining := runTool[[]struct {
		DraftID string `json:"draft_id"`
	}](t, ToolListDrafts, h.listDrafts, account)
	assert.Empty(remaining)

	// delete_label is not supported over IMAP and must surface a clear error.
	deleteResult := runToolExpectError(t, ToolDeleteLabel, h.deleteLabel, map[string]any{
		"account":  testIMAPAccount,
		"label_id": "Recruiting",
	})
	assert.Contains(resultText(t, deleteResult), "IMAP does not support")
}

// TestDraftToolsWithoutFactoryReportError verifies the write handlers fail
// with a clear message when no live mail factory is configured.
func TestDraftToolsWithoutFactoryReportError(t *testing.T) {
	h := &handlers{}
	r := runToolExpectError(t, ToolListDrafts, h.listDrafts, map[string]any{"account": testIMAPAccount})
	assert.Contains(t, resultText(t, r), "live mail API not configured")
}

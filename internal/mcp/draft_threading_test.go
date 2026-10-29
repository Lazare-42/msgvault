package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

const threadingParentRaw = "From: Alice Example <alice@example.com>\r\n" +
	"To: user@example.com, Bob Example <bob@example.com>\r\n" +
	"Cc: carol@example.com\r\n" +
	"Subject: Quarterly plan\r\n" +
	"Message-ID: <parent@example.com>\r\n" +
	"In-Reply-To: <earlier@example.com>\r\n" +
	"References: <root@example.com> <earlier@example.com>\r\n" +
	"Date: Mon, 26 Oct 2026 09:00:00 +0100\r\n" +
	"\r\n" +
	"Here is the plan.\r\n"

type threadingDraftResult struct {
	DraftID    string   `json:"draft_id"`
	Subject    string   `json:"subject"`
	To         []string `json:"to"`
	Cc         []string `json:"cc"`
	InReplyTo  string   `json:"in_reply_to"`
	References string   `json:"references"`
}

// A reply built from an archived message carries the full References chain
// and keeps it through update_draft, which rebuilds the IMAP message.
func TestCreateDraftReplyToArchivedMessageOverIMAP(t *testing.T) {
	assert := assert.New(t)
	port := startIMAPTestServer(t)
	engine := &querytest.MockEngine{
		Messages:    map[int64]*query.MessageDetail{42: {ID: 42, SourceID: 1, Subject: "Quarterly plan"}},
		RawMessages: map[int64][]byte{42: []byte(threadingParentRaw)},
		Accounts:    []query.AccountInfo{{ID: 1, SourceType: "imap", Identifier: testIMAPAccount}},
	}
	h := &handlers{engine: engine, gmailFactory: imapFactory(t, port)}
	const wantReferences = "<root@example.com> <earlier@example.com> <parent@example.com>"

	created := runTool[threadingDraftResult](t, ToolCreateDraft, h.createDraft, map[string]any{
		"account": testIMAPAccount, "reply_to_message_id": float64(42), "body": "Looks good.",
	})
	require.NotEmpty(t, created.DraftID)
	assert.Equal("<parent@example.com>", created.InReplyTo)
	assert.Equal(wantReferences, created.References)
	assert.Equal("Re: Quarterly plan", created.Subject)
	assert.Equal([]string{`"Alice Example" <alice@example.com>`}, created.To)
	assert.Empty(created.Cc)

	stored := runTool[gmail.ComposedDraft](t, ToolGetDraft, h.getDraft, map[string]any{
		"account": testIMAPAccount, "draft_id": created.DraftID,
	})
	assert.Equal("<parent@example.com>", stored.Message.InReplyTo)
	assert.Equal(wantReferences, stored.Message.References)

	updated := runTool[struct {
		DraftID string `json:"draft_id"`
	}](t, ToolUpdateDraft, h.updateDraft, map[string]any{
		"account": testIMAPAccount, "draft_id": created.DraftID, "to": "alice@example.com",
		"subject": "Re: Quarterly plan", "body": "Looks good, with one change.",
	})
	stored = runTool[gmail.ComposedDraft](t, ToolGetDraft, h.getDraft, map[string]any{
		"account": testIMAPAccount, "draft_id": updated.DraftID,
	})
	assert.Equal("<parent@example.com>", stored.Message.InReplyTo, "update_draft must keep threading")
	assert.Equal(wantReferences, stored.Message.References)
}

func TestCreateDraftReplyAllExcludesAccount(t *testing.T) {
	port := startIMAPTestServer(t)
	engine := &querytest.MockEngine{
		Messages:    map[int64]*query.MessageDetail{42: {ID: 42, SourceID: 1}},
		RawMessages: map[int64][]byte{42: []byte(threadingParentRaw)},
	}
	h := &handlers{engine: engine, gmailFactory: imapFactory(t, port)}

	created := runTool[threadingDraftResult](t, ToolCreateDraft, h.createDraft, map[string]any{
		"account": testIMAPAccount, "reply_to_message_id": float64(42), "reply_all": true, "body": "Thanks all.",
	})
	assert.Equal(t, []string{`"Alice Example" <alice@example.com>`, `"Bob Example" <bob@example.com>`}, created.To)
	assert.Equal(t, []string{"<carol@example.com>"}, created.Cc)
}

func TestCreateDraftReplyRejectsManualThreading(t *testing.T) {
	port := startIMAPTestServer(t)
	engine := &querytest.MockEngine{
		Messages:    map[int64]*query.MessageDetail{42: {ID: 42, SourceID: 1}},
		RawMessages: map[int64][]byte{42: []byte(threadingParentRaw)},
	}
	h := &handlers{engine: engine, gmailFactory: imapFactory(t, port)}

	result := runToolExpectError(t, ToolCreateDraft, h.createDraft, map[string]any{
		"account": testIMAPAccount, "reply_to_message_id": float64(42),
		"in_reply_to": "<other@example.com>", "body": "x",
	})
	assert.Contains(t, resultText(t, result), "omit in_reply_to")
}

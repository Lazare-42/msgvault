package mcp

import (
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Exercise the real store, daemon HTTP response, generated client, engine adapter,
// and MCP serialization: testing a query engine alone misses adapter data loss.
func TestDaemonGetMessagePreservesReplyHeaders(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "reader@example.test")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "INBOX|1", "Synthetic")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "INBOX|1", MessageType: "email", RFC822MessageID: sql.NullString{String: "<Case-Sensitive-ID@example.test>", Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Reply body", Valid: true}, sql.NullString{String: "<p>Reply body</p>", Valid: true}))
	local := query.NewEngine(st.DB(), st.IsPostgreSQL())
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Engine: local, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	remote, err := daemonclient.NewEngine(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = remote.Close() })
	expected, err := local.GetMessage(t.Context(), id)
	requirements.NoError(err)
	actual, err := remote.GetMessage(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal(expected.RFC822MessageID, actual.RFC822MessageID)
	assertions.Equal(expected.BodyText, actual.BodyText)
	assertions.Equal(expected.BodyHTML, actual.BodyHTML)
	for name, engine := range map[string]query.Engine{"local": local, "daemon": remote} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			result := rawCallTool(t, ServeOptions{Engine: engine}, ToolGetMessage, map[string]any{"id": id})
			detail, ok := result["structuredContent"].(map[string]any)
			requirements.True(ok, "%#v", result)
			assertions.Equal("<Case-Sensitive-ID@example.test>", detail["rfc822_message_id"])
		})
	}
}

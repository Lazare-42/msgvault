package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

// stubWhatsAppClient satisfies whatsapplive.Client for catalog tests that
// never invoke the live session.
type stubWhatsAppClient struct{ whatsapplive.Client }

func TestWhatsAppLiveMCPServesOnlyWhatsAppTools(t *testing.T) {
	f := storetest.New(t)
	opts := whatsAppLiveMCPOptions(
		query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()), f.Store, stubWhatsAppClient{},
		t.TempDir(), t.TempDir(), "https://example.test/personal/qr",
	)
	assert.Equal(t, "https://example.test/personal/qr", opts.WhatsAppLoginURL)

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion": "2026-07-28",
				"io.modelcontextprotocol/clientInfo": map[string]any{
					"name": "msgvault-test", "version": "1.0.0",
				},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/list")
	recorder := httptest.NewRecorder()
	mcpserver.NewStreamableHTTPHandler(opts, true).ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	require.Empty(t, response.Error, recorder.Body.String())

	got := make([]string, 0, len(response.Result.Tools))
	for _, tool := range response.Result.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	want := mcpserver.WhatsAppToolNames()
	sort.Strings(want)
	assert.Equal(t, want, got)
}

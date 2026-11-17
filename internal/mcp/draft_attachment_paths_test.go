package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// attachmentPathFixture lays out an allowed per-session folder, matched by a
// glob the way agent scratchpads are, and a sibling folder outside it.
type attachmentPathFixture struct {
	pattern string
	inside  string
	outside string
}

func newAttachmentPathFixture(t *testing.T) attachmentPathFixture {
	t.Helper()
	base := t.TempDir()
	inside := filepath.Join(base, "sessions", "abc123", "scratchpad")
	outside := filepath.Join(base, "secrets")
	require.NoError(t, os.MkdirAll(inside, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	return attachmentPathFixture{
		pattern: filepath.Join(base, "sessions", "*", "scratchpad"),
		inside:  inside,
		outside: outside,
	}
}

func TestResolveDraftAttachmentsReadsAllowedPaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentPathFixture(t)
	invoice := []byte("%PDF-1.4 invoice bytes")
	notes := []byte("plain notes")
	require.NoError(os.WriteFile(filepath.Join(f.inside, "invoice-1.pdf"), invoice, 0o600))
	require.NoError(os.WriteFile(filepath.Join(f.inside, "notes.unknownext"), notes, 0o600))
	h := &handlers{draftAttachmentDirs: []string{f.pattern}}

	atts, err := h.resolveDraftAttachments(t.Context(), map[string]any{
		"attachment_paths": []any{
			filepath.Join(f.inside, "invoice-1.pdf"),
			filepath.Join(f.inside, "notes.unknownext"),
		},
	})
	require.NoError(err)
	require.Len(atts, 2)
	assert.Equal("invoice-1.pdf", atts[0].Filename)
	assert.Equal("application/pdf", atts[0].ContentType)
	assert.Equal(invoice, atts[0].Content)
	assert.Equal("notes.unknownext", atts[1].Filename)
	assert.Equal("text/plain; charset=utf-8", atts[1].ContentType, "unknown extensions fall back to sniffing")
}

func TestResolveDraftAttachmentsRejectsUnsafePaths(t *testing.T) {
	f := newAttachmentPathFixture(t)
	secret := filepath.Join(f.outside, "id_ed25519")
	require.NoError(t, os.WriteFile(secret, []byte("private key"), 0o600))
	link := filepath.Join(f.inside, "looks-harmless.pdf")
	require.NoError(t, os.Symlink(secret, link))
	sub := filepath.Join(f.inside, "folder")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	tests := []struct {
		name string
		dirs []string
		path any
		want string
	}{
		{"disabled without attachment_dirs", nil, filepath.Join(f.inside, "x.pdf"), "attaching local files is disabled"},
		{"outside the allowed folders", []string{f.pattern}, secret, "outside the configured attachment_dirs"},
		{"symlink escaping the folder", []string{f.pattern}, link, "outside the configured attachment_dirs"},
		{"parent traversal", []string{f.pattern}, filepath.Join(f.inside, "..", "..", "..", "secrets", "id_ed25519"), "outside the configured attachment_dirs"},
		{"relative path", []string{f.pattern}, "invoice.pdf", "is not an absolute path"},
		{"directory", []string{f.pattern}, sub, "is not a regular file"},
		{"missing file", []string{f.pattern}, filepath.Join(f.inside, "missing.pdf"), "resolve"},
		{"non-string item", []string{f.pattern}, 42, "must be a non-empty string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &handlers{draftAttachmentDirs: tc.dirs}
			_, err := h.resolveDraftAttachments(t.Context(), map[string]any{"attachment_paths": []any{tc.path}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestResolveDraftAttachmentsBoundsPathSizes(t *testing.T) {
	f := newAttachmentPathFixture(t)
	big := filepath.Join(f.inside, "big.bin")
	require.NoError(t, os.WriteFile(big, nil, 0o600))
	require.NoError(t, os.Truncate(big, maxDraftAttachmentsSize/2+1))
	h := &handlers{draftAttachmentDirs: []string{f.pattern}}

	_, err := h.resolveDraftAttachments(t.Context(), map[string]any{"attachment_paths": []any{big, big}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "total attachment size exceeds")
}

// A path attachment must reach the provider's draft intact, not only the
// in-memory attachment list.
func TestCreateDraftWithAttachmentPathOverIMAP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttachmentPathFixture(t)
	content := []byte("%PDF-1.4 invoice that is too large to pass inline\n")
	path := filepath.Join(f.inside, "invoice-4888.pdf")
	require.NoError(os.WriteFile(path, content, 0o600))
	port := startIMAPTestServer(t)
	h := &handlers{gmailFactory: imapFactory(t, port), draftAttachmentDirs: []string{f.pattern}}

	created := runTool[struct {
		DraftID     string `json:"draft_id"`
		Attachments int    `json:"attachments"`
	}](t, ToolCreateDraft, h.createDraft, map[string]any{
		"account":          testIMAPAccount,
		"to":               "billing@example.com",
		"subject":          "Invoices",
		"body":             "Invoices attached.",
		"attachment_paths": []any{path},
	})
	require.NotEmpty(created.DraftID)
	assert.Equal(1, created.Attachments)

	client, err := imapFactory(t, port)(context.Background(), testIMAPAccount)
	require.NoError(err)
	defer func() { _ = client.Close() }()
	raw, err := client.GetMessageRaw(context.Background(), created.DraftID)
	require.NoError(err)
	parsed, err := msgmime.Parse(raw.Raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 1)
	assert.Equal("invoice-4888.pdf", parsed.Attachments[0].Filename)
	assert.Equal("application/pdf", parsed.Attachments[0].ContentType)
	assert.Equal(content, parsed.Attachments[0].Content)
}

func TestDraftSchemasIncludeAttachmentPaths(t *testing.T) {
	require := require.New(t)
	opts := ServeOptions{
		Engine: &querytest.MockEngine{},
		GmailFactory: func(context.Context, string) (gmail.LiveAPI, error) {
			return nil, errors.New("not used in this schema-only test")
		},
	}
	byName := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{ToolCreateDraft, ToolUpdateDraft} {
		tool, ok := byName[name]
		require.True(ok, "%s tool registered", name)
		schema, ok := tool["inputSchema"].(map[string]any)
		require.True(ok)
		properties, ok := schema["properties"].(map[string]any)
		require.True(ok)
		paths, ok := properties["attachment_paths"].(map[string]any)
		require.True(ok, "%s attachment_paths property: %#v", name, properties)
		assert.Equal(t, "array", paths["type"])
		assert.Equal(t, map[string]any{"type": "string"}, paths["items"])
	}
}

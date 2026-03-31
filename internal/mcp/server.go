package mcp

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wesm/msgvault/internal/gmail"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/vector"
	"github.com/wesm/msgvault/internal/vector/hybrid"
)

// GmailClientFactory creates authenticated Gmail API clients for a given
// account email. Returns nil if Gmail draft operations are not available
// (e.g., OAuth not configured). The caller is responsible for closing the client.
type GmailClientFactory func(ctx context.Context, email string) (*gmail.Client, error)

// Tool name constants.
const (
	ToolSearchMessages      = "search_messages"
	ToolGetMessage          = "get_message"
	ToolGetAttachment       = "get_attachment"
	ToolExportAttachment    = "export_attachment"
	ToolListMessages        = "list_messages"
	ToolGetStats            = "get_stats"
	ToolAggregate           = "aggregate"
	ToolStageDeletion       = "stage_deletion"
	ToolFindSimilarMessages = "find_similar_messages"
	ToolListDrafts          = "list_drafts"
	ToolGetDraft            = "get_draft"
	ToolCreateDraft         = "create_draft"
	ToolUpdateDraft         = "update_draft"
	ToolDeleteDraft         = "delete_draft"
	ToolSendDraft           = "send_draft"
	ToolModifyLabels        = "modify_labels"
	ToolCreateLabel         = "create_label"
	ToolDeleteLabel         = "delete_label"
	ToolListGmailLabels     = "list_gmail_labels"
)

// Common argument helpers for recurring tool option definitions.

func withLimit(defaultDesc string) mcp.ToolOption {
	return mcp.WithNumber("limit",
		mcp.Description("Maximum results to return (default "+defaultDesc+")"),
	)
}

func withOffset() mcp.ToolOption {
	return mcp.WithNumber("offset",
		mcp.Description("Number of results to skip for pagination (default 0)"),
	)
}

func withAfter() mcp.ToolOption {
	return mcp.WithString("after",
		mcp.Description("Only messages after this date (YYYY-MM-DD)"),
	)
}

func withBefore() mcp.ToolOption {
	return mcp.WithString("before",
		mcp.Description("Only messages before this date (YYYY-MM-DD)"),
	)
}

func withAccount() mcp.ToolOption {
	return mcp.WithString("account",
		mcp.Description("Filter by account email address (use get_stats to list available accounts)"),
	)
}

// ServeOptions configures an MCP server. Only Engine is required; the
// HybridEngine and VectorCfg fields enable the vector/hybrid modes on
// the search_messages tool, and Backend additionally enables the
// find_similar_messages tool.
type ServeOptions struct {
	Engine         query.Engine
	AttachmentsDir string
	DataDir        string

	// HybridEngine is optional. When nil, search_messages rejects
	// mode=vector and mode=hybrid with a vector_not_enabled error.
	HybridEngine *hybrid.Engine
	// VectorCfg should already have ApplyDefaults() called on it.
	VectorCfg vector.Config
	// Backend is optional. When nil, find_similar_messages rejects all
	// calls with a vector_not_enabled error.
	Backend vector.Backend
	// GmailFactory is optional. When non-nil, draft management tools are exposed.
	GmailFactory GmailClientFactory
}

// BuildMCPServer creates an MCPServer with all tools registered.
// Callers choose the transport (stdio, SSE, etc.).
func BuildMCPServer(opts ServeOptions) *server.MCPServer {
	s := server.NewMCPServer(
		"msgvault",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	h := &handlers{
		engine:         opts.Engine,
		attachmentsDir: opts.AttachmentsDir,
		dataDir:        opts.DataDir,
		hybridEngine:   opts.HybridEngine,
		vectorCfg:      opts.VectorCfg,
		backend:        opts.Backend,
		gmailFactory:   opts.GmailFactory,
	}

	vectorAvailable := opts.HybridEngine != nil
	s.AddTool(searchMessagesTool(vectorAvailable), h.searchMessages)
	s.AddTool(getMessageTool(), h.getMessage)
	s.AddTool(getAttachmentTool(), h.getAttachment)
	s.AddTool(exportAttachmentTool(), h.exportAttachment)
	s.AddTool(listMessagesTool(), h.listMessages)
	s.AddTool(getStatsTool(), h.getStats)
	s.AddTool(aggregateTool(), h.aggregate)
	s.AddTool(stageDeletionTool(), h.stageDeletion)
	if opts.Backend != nil {
		s.AddTool(findSimilarMessagesTool(), h.findSimilarMessages)
	}

	if opts.GmailFactory != nil {
		s.AddTool(listDraftsTool(), h.listDrafts)
		s.AddTool(getDraftTool(), h.getDraft)
		s.AddTool(createDraftTool(), h.createDraft)
		s.AddTool(updateDraftTool(), h.updateDraft)
		s.AddTool(deleteDraftTool(), h.deleteDraft)
		s.AddTool(sendDraftTool(), h.sendDraft)
		s.AddTool(modifyLabelsTool(), h.modifyLabels)
		s.AddTool(createLabelTool(), h.createLabel)
		s.AddTool(deleteLabelTool(), h.deleteLabel)
		s.AddTool(listGmailLabelsTool(), h.listGmailLabels)
	}

	return s
}

// Serve creates an MCP server with email archive tools and serves over stdio.
// It blocks until stdin is closed or the context is cancelled.
func Serve(ctx context.Context, engine query.Engine, attachmentsDir, dataDir string, gmailFactory GmailClientFactory) error {
	return ServeWithOptions(ctx, ServeOptions{
		Engine:         engine,
		AttachmentsDir: attachmentsDir,
		DataDir:        dataDir,
		GmailFactory:   gmailFactory,
	})
}

// ServeWithOptions creates an MCP server from opts and serves over stdio.
// It blocks until stdin is closed or the context is cancelled.
func ServeWithOptions(ctx context.Context, opts ServeOptions) error {
	s := BuildMCPServer(opts)
	stdio := server.NewStdioServer(s)
	return stdio.Listen(ctx, os.Stdin, os.Stdout)
}

// ServeStreamableHTTP starts the MCP server over Streamable HTTP transport.
// If apiKey is non-empty, requests must include a Bearer token matching the key.
// It blocks until the context is cancelled or the server fails to start.
func ServeStreamableHTTP(ctx context.Context, addr, apiKey string, srvOpts ServeOptions) error {
	mcpServer := BuildMCPServer(srvOpts)

	opts := []server.StreamableHTTPOption{
		server.WithHeartbeatInterval(30 * time.Second),
	}

	// When an API key is set, wrap the MCP handler with bearer token auth.
	if apiKey != "" {
		opts = append(opts, server.WithStreamableHTTPServer(&http.Server{
			Addr:    addr,
			Handler: nil, // set below after NewStreamableHTTPServer
		}))
	}

	httpServer := server.NewStreamableHTTPServer(mcpServer, opts...)

	// If API key is configured, wrap the default handler with auth middleware.
	if apiKey != "" {
		mux := http.NewServeMux()
		mux.Handle("/mcp", requireBearer(apiKey, httpServer))
		httpServer = server.NewStreamableHTTPServer(mcpServer,
			server.WithHeartbeatInterval(30*time.Second),
			server.WithStreamableHTTPServer(&http.Server{
				Addr:    addr,
				Handler: mux,
			}),
		)
	}

	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	return httpServer.Start(addr)
}

// requireBearer wraps an http.Handler with Bearer token authentication.
func requireBearer(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if !strings.HasPrefix(auth, "Bearer ") || token != apiKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func searchMessagesTool(vectorAvailable bool) mcp.Tool {
	if !vectorAvailable {
		return mcp.NewTool(ToolSearchMessages,
			mcp.WithDescription("Search emails using Gmail-like query syntax. Supports from:, to:, subject:, label:, has:attachment, before:, after:, and free text. (This server is not configured for vector search; only keyword FTS is available.)"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Gmail-style search query (e.g. 'from:alice subject:meeting after:2024-01-01')"),
			),
			withAccount(),
			withLimit("20"),
			withOffset(),
		)
	}
	return mcp.NewTool(ToolSearchMessages,
		mcp.WithDescription("Search emails using Gmail-like query syntax. Supports from:, to:, subject:, label:, has:attachment, before:, after:, and free text. Vector search is configured: set mode=vector for pure semantic search or mode=hybrid to fuse BM25 and vector ranking via RRF. Vector/hybrid modes require free-text terms in the query; filter-only queries must use mode=fts."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Gmail-style search query (e.g. 'from:alice subject:meeting after:2024-01-01'); mode=vector|hybrid require at least one free-text term"),
		),
		withAccount(),
		withLimit("20"),
		// offset is FTS-only here. Vector/hybrid responses don't page —
		// callers should bump limit (capped by max_page_size_hybrid) instead.
		mcp.WithNumber("offset",
			mcp.Description("Number of results to skip for pagination (default 0). Only valid for mode=fts; mode=vector and mode=hybrid reject offset>0 with pagination_unsupported."),
		),
		mcp.WithString("mode",
			mcp.Description("Search mode: fts (default, keyword only), vector (semantic only), or hybrid (BM25 + vector fused via RRF)"),
			mcp.Enum("fts", "vector", "hybrid"),
		),
		mcp.WithBoolean("explain",
			mcp.Description("Include per-signal scores in the response (for debugging or ranking inspection)"),
		),
	)
}

func getMessageTool() mcp.Tool {
	return mcp.NewTool(ToolGetMessage,
		mcp.WithDescription("Get full message details including body text, recipients, labels, and attachments by message ID."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithNumber("id",
			mcp.Required(),
			mcp.Description("Message ID"),
		),
	)
}

func getAttachmentTool() mcp.Tool {
	return mcp.NewTool(ToolGetAttachment,
		mcp.WithDescription("Get attachment content by attachment ID. Returns metadata as text and the file content as an embedded resource blob. Use get_message first to find attachment IDs."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithNumber("attachment_id",
			mcp.Required(),
			mcp.Description("Attachment ID (from get_message response)"),
		),
	)
}

func exportAttachmentTool() mcp.Tool {
	return mcp.NewTool(ToolExportAttachment,
		mcp.WithDescription("Save an attachment to the local filesystem. Use this for file types that cannot be displayed inline (e.g. PDFs, documents). Returns the saved file path."),
		mcp.WithNumber("attachment_id",
			mcp.Required(),
			mcp.Description("Attachment ID (from get_message response)"),
		),
		mcp.WithString("destination",
			mcp.Description("Directory to save the file to (default: ~/Downloads)"),
		),
	)
}

func listMessagesTool() mcp.Tool {
	return mcp.NewTool(ToolListMessages,
		mcp.WithDescription("List messages with optional filters. Returns message summaries sorted by date."),
		mcp.WithReadOnlyHintAnnotation(true),
		withAccount(),
		mcp.WithString("from",
			mcp.Description("Filter by sender email address"),
		),
		mcp.WithString("to",
			mcp.Description("Filter by recipient email address"),
		),
		mcp.WithString("label",
			mcp.Description("Filter by Gmail label"),
		),
		withAfter(),
		withBefore(),
		mcp.WithBoolean("has_attachment",
			mcp.Description("Only messages with attachments"),
		),
		withLimit("20"),
		withOffset(),
	)
}

func getStatsTool() mcp.Tool {
	return mcp.NewTool(ToolGetStats,
		mcp.WithDescription("Get archive overview: total messages, size, attachment count, and accounts."),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func aggregateTool() mcp.Tool {
	return mcp.NewTool(ToolAggregate,
		mcp.WithDescription("Get grouped statistics (e.g. top senders, domains, labels, or message volume over time)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("group_by",
			mcp.Required(),
			mcp.Description("Dimension to group by"),
			mcp.Enum("sender", "recipient", "domain", "label", "time"),
		),
		withAccount(),
		withLimit("50"),
		withAfter(),
		withBefore(),
	)
}

func stageDeletionTool() mcp.Tool {
	return mcp.NewTool(ToolStageDeletion,
		mcp.WithDescription("Stage messages for deletion. Use EITHER 'query' (Gmail-style search) OR structured filters (from, domain, label, etc.), not both. Does NOT delete immediately - run 'msgvault delete-staged' CLI command to execute staged deletions."),
		withAccount(),
		mcp.WithString("query",
			mcp.Description("Gmail-style search query (e.g. 'from:linkedin subject:job alert'). Cannot be combined with structured filters."),
		),
		mcp.WithString("from",
			mcp.Description("Filter by sender email address"),
		),
		mcp.WithString("domain",
			mcp.Description("Filter by sender domain (e.g. 'linkedin.com')"),
		),
		mcp.WithString("label",
			mcp.Description("Filter by Gmail label (e.g. 'CATEGORY_PROMOTIONS')"),
		),
		withAfter(),
		withBefore(),
		mcp.WithBoolean("has_attachment",
			mcp.Description("Only messages with attachments"),
		),
	)
}

func findSimilarMessagesTool() mcp.Tool {
	return mcp.NewTool(ToolFindSimilarMessages,
		mcp.WithDescription("Find messages whose embeddings are closest to the given message. Requires vector search to be configured and an active index generation."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithNumber("message_id",
			mcp.Required(),
			mcp.Description("Seed message ID; its embedding is used as the query vector"),
		),
		withLimit("20"),
		withAccount(),
		withAfter(),
		withBefore(),
		mcp.WithBoolean("has_attachment",
			mcp.Description("Only messages with attachments"),
		),
	)
}

func listDraftsTool() mcp.Tool {
	return mcp.NewTool(ToolListDrafts,
		mcp.WithDescription("List email drafts from Gmail. Returns draft ID, subject, recipients, and body preview for each draft."),
		mcp.WithReadOnlyHintAnnotation(true),
		withAccount(),
		mcp.WithString("query",
			mcp.Description("Optional Gmail-style search query to filter drafts (e.g. 'subject:meeting')"),
		),
		withLimit("20"),
	)
}

func getDraftTool() mcp.Tool {
	return mcp.NewTool(ToolGetDraft,
		mcp.WithDescription("Get full details of a single Gmail draft by draft ID, including body text, recipients, and subject."),
		mcp.WithReadOnlyHintAnnotation(true),
		withAccount(),
		mcp.WithString("draft_id",
			mcp.Required(),
			mcp.Description("Draft ID (from list_drafts response)"),
		),
	)
}

func createDraftTool() mcp.Tool {
	return mcp.NewTool(ToolCreateDraft,
		mcp.WithDescription("Create a new email draft in Gmail. The draft is saved but NOT sent. Use send_draft to send it. For rich formatting (links, bold, etc.), set content_type to text/html and provide HTML body."),
		withAccount(),
		mcp.WithString("to",
			mcp.Description("Recipient email address(es), comma-separated"),
		),
		mcp.WithString("cc",
			mcp.Description("CC recipients, comma-separated"),
		),
		mcp.WithString("bcc",
			mcp.Description("BCC recipients, comma-separated"),
		),
		mcp.WithString("subject",
			mcp.Description("Email subject line"),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("Email body content. Plain text by default, or HTML when content_type is text/html"),
		),
		mcp.WithString("content_type",
			mcp.Description("Body content type: 'text/plain' (default) or 'text/html' for rich formatting with links, bold, etc."),
		),
		mcp.WithString("thread_id",
			mcp.Description("Thread ID to create a reply draft within an existing thread"),
		),
	)
}

func updateDraftTool() mcp.Tool {
	return mcp.NewTool(ToolUpdateDraft,
		mcp.WithDescription("Update an existing Gmail draft. Replaces the entire draft content."),
		withAccount(),
		mcp.WithString("draft_id",
			mcp.Required(),
			mcp.Description("Draft ID to update (from list_drafts response)"),
		),
		mcp.WithString("to",
			mcp.Description("Recipient email address(es), comma-separated"),
		),
		mcp.WithString("cc",
			mcp.Description("CC recipients, comma-separated"),
		),
		mcp.WithString("bcc",
			mcp.Description("BCC recipients, comma-separated"),
		),
		mcp.WithString("subject",
			mcp.Description("Email subject line"),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("Email body content. Plain text by default, or HTML when content_type is text/html"),
		),
		mcp.WithString("content_type",
			mcp.Description("Body content type: 'text/plain' (default) or 'text/html' for rich formatting with links, bold, etc."),
		),
		mcp.WithString("thread_id",
			mcp.Description("Thread ID for reply drafts"),
		),
	)
}

func deleteDraftTool() mcp.Tool {
	return mcp.NewTool(ToolDeleteDraft,
		mcp.WithDescription("Permanently delete a Gmail draft."),
		withAccount(),
		mcp.WithString("draft_id",
			mcp.Required(),
			mcp.Description("Draft ID to delete"),
		),
	)
}

func sendDraftTool() mcp.Tool {
	return mcp.NewTool(ToolSendDraft,
		mcp.WithDescription("Send an existing Gmail draft. The draft is removed and a sent message is created. This action cannot be undone."),
		withAccount(),
		mcp.WithString("draft_id",
			mcp.Required(),
			mcp.Description("Draft ID to send"),
		),
	)
}

func modifyLabelsTool() mcp.Tool {
	return mcp.NewTool(ToolModifyLabels,
		mcp.WithDescription("Add and/or remove Gmail labels on messages. Use this to label, archive (remove INBOX), mark read (remove UNREAD), star, or categorize messages. Provide Gmail message IDs (from search_messages source_message_id field). Use list_gmail_labels to find label IDs."),
		withAccount(),
		mcp.WithString("message_ids",
			mcp.Required(),
			mcp.Description("Comma-separated Gmail message IDs (the source_message_id from search results, NOT the archive numeric ID)"),
		),
		mcp.WithString("add_labels",
			mcp.Description("Comma-separated label IDs to add (e.g. 'STARRED,Label_123')"),
		),
		mcp.WithString("remove_labels",
			mcp.Description("Comma-separated label IDs to remove (e.g. 'INBOX,UNREAD'). Remove INBOX to archive."),
		),
	)
}

func createLabelTool() mcp.Tool {
	return mcp.NewTool(ToolCreateLabel,
		mcp.WithDescription("Create a new Gmail label. Returns the label ID which can be used with modify_labels."),
		withAccount(),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Label name. Use '/' for nesting (e.g. 'Projects/Acme')"),
		),
	)
}

func deleteLabelTool() mcp.Tool {
	return mcp.NewTool(ToolDeleteLabel,
		mcp.WithDescription("Permanently delete a Gmail label by ID. Messages with this label are NOT deleted; the label is simply removed from them. Only user-created labels can be deleted (not system labels like INBOX, SENT, etc.). Use list_gmail_labels to find label IDs."),
		withAccount(),
		mcp.WithString("label_id",
			mcp.Required(),
			mcp.Description("Label ID to delete (e.g. 'Label_11'). Use list_gmail_labels to find IDs."),
		),
	)
}

func listGmailLabelsTool() mcp.Tool {
	return mcp.NewTool(ToolListGmailLabels,
		mcp.WithDescription("List all Gmail labels from the live account (not the archive). Returns label IDs and names for use with modify_labels."),
		mcp.WithReadOnlyHintAnnotation(true),
		withAccount(),
	)
}

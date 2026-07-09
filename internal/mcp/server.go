package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/googledocs"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

// GmailClientFactory creates authenticated Gmail API clients for a given
// account email. Returns nil if Gmail draft operations are not available
// (e.g., OAuth not configured). The caller is responsible for closing the client.
type GmailClientFactory func(ctx context.Context, email string) (*gmail.Client, error)

// WhatsAppClientFactory creates a live WhatsApp client for an archive account.
type WhatsAppClientFactory func(ctx context.Context, account string) (whatsapplive.Client, error)

// GoogleDocsClientFactory creates an authenticated Google Docs client for
// configured Drive folder sources.
type GoogleDocsClientFactory func(ctx context.Context) (googledocs.Client, error)

// Tool name constants.
const (
	ToolSearchMessages       = "search_messages"
	ToolGetMessage           = "get_message"
	ToolGetAttachment        = "get_attachment"
	ToolExportAttachment     = "export_attachment"
	ToolListMessages         = "list_messages"
	ToolGetStats             = "get_stats"
	ToolAggregate            = "aggregate"
	ToolStageDeletion        = "stage_deletion"
	ToolSearchByDomains      = "search_by_domains"
	ToolFindSimilarMessages  = "find_similar_messages"
	ToolListDrafts           = "list_drafts"
	ToolGetDraft             = "get_draft"
	ToolCreateDraft          = "create_draft"
	ToolUpdateDraft          = "update_draft"
	ToolDeleteDraft          = "delete_draft"
	ToolSendDraft            = "send_draft"
	ToolModifyLabels         = "modify_labels"
	ToolCreateLabel          = "create_label"
	ToolDeleteLabel          = "delete_label"
	ToolListGmailLabels      = "list_gmail_labels"
	ToolWhatsAppStatus       = "whatsapp_status"
	ToolWhatsAppStartLogin   = "whatsapp_start_login"
	ToolWhatsAppLoginStatus  = "whatsapp_login_status"
	ToolWhatsAppLogout       = "whatsapp_logout"
	ToolSendWhatsAppMessage  = "send_whatsapp_message"
	ToolSendWhatsAppReaction = "send_whatsapp_reaction"
	ToolListGoogleDocs       = "list_google_docs"
	ToolSearchGoogleDocs     = "search_google_docs"
	ToolGetGoogleDoc         = "get_google_doc"
	ToolAppendGoogleDocText  = "append_google_doc_text"
	ToolReplaceGoogleDocText = "replace_google_doc_text"
)

// search_messages mode values (wire format).
const (
	searchModeFTS    = "fts"
	searchModeVector = "vector"
	searchModeHybrid = "hybrid"
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
	// WhatsAppFactory is optional. When non-nil, live WhatsApp tools are exposed.
	WhatsAppFactory WhatsAppClientFactory
	// WhatsAppLoginURL is an optional browser URL for QR login/resync.
	WhatsAppLoginURL string
	// GoogleDocsFactory is optional. When non-nil, Google Docs tools are exposed.
	GoogleDocsFactory GoogleDocsClientFactory
}

// BuildMCPServer builds an MCP server with all tools registered from opts.
// Shared by ServeWithOptions (stdio), ServeHTTPWithOptions (HTTP), and the
// SSE transport. Callers choose the transport.
func BuildMCPServer(opts ServeOptions) *server.MCPServer {
	s := server.NewMCPServer(
		"msgvault",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	h := &handlers{
		engine:            opts.Engine,
		attachmentsDir:    opts.AttachmentsDir,
		dataDir:           opts.DataDir,
		hybridEngine:      opts.HybridEngine,
		vectorCfg:         opts.VectorCfg,
		backend:           opts.Backend,
		gmailFactory:      opts.GmailFactory,
		whatsAppFactory:   opts.WhatsAppFactory,
		whatsAppLoginURL:  strings.TrimSpace(opts.WhatsAppLoginURL),
		googleDocsFactory: opts.GoogleDocsFactory,
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
	s.AddTool(searchByDomainsTool(), h.searchByDomains)
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

	if opts.WhatsAppFactory != nil {
		s.AddTool(whatsAppStatusTool(), h.whatsAppStatus)
		s.AddTool(whatsAppStartLoginTool(), h.whatsAppStartLogin)
		s.AddTool(whatsAppLoginStatusTool(), h.whatsAppLoginStatus)
		s.AddTool(whatsAppLogoutTool(), h.whatsAppLogout)
		s.AddTool(sendWhatsAppMessageTool(), h.sendWhatsAppMessage)
		s.AddTool(sendWhatsAppReactionTool(), h.sendWhatsAppReaction)
	}

	if opts.GoogleDocsFactory != nil {
		s.AddTool(listGoogleDocsTool(), h.listGoogleDocs)
		s.AddTool(searchGoogleDocsTool(), h.searchGoogleDocs)
		s.AddTool(getGoogleDocTool(), h.getGoogleDoc)
		s.AddTool(appendGoogleDocTextTool(), h.appendGoogleDocText)
		s.AddTool(replaceGoogleDocTextTool(), h.replaceGoogleDocText)
	}

	return s
}

// Serve creates an MCP server with email archive tools and serves over stdio.
// It blocks until stdin is closed or the context is cancelled.
// dataDir is the base data directory (e.g., ~/.msgvault) used for deletions.
//
// Serve is a thin wrapper around ServeWithOptions that leaves the vector
// fields empty; callers that want vector/hybrid search should use
// ServeWithOptions directly.
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
	if err := stdio.Listen(ctx, os.Stdin, os.Stdout); err != nil {
		return fmt.Errorf("serve MCP over stdio: %w", err)
	}
	return nil
}

// ServeHTTPWithOptions creates an MCP server from opts and serves over
// StreamableHTTP on the given address. Useful for daemonized deployments
// where remote MCP clients (Claude Desktop, IDE plugins, custom
// integrations) connect over a network rather than a local stdin/stdout
// pipe.
//
// When ctx is canceled (e.g. on SIGINT in the daemon), the HTTP server
// is shut down gracefully via httpServer.Shutdown so in-flight requests
// can complete. Mirrors how ServeWithOptions threads the context through
// the stdio Listen call.
func ServeHTTPWithOptions(ctx context.Context, opts ServeOptions, addr string) error {
	s := BuildMCPServer(opts)
	httpServer := server.NewStreamableHTTPServer(s)
	fmt.Fprintf(os.Stderr, "Starting MCP server on %s\n", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Graceful shutdown with a short bound; in-flight tool calls
		// usually finish in milliseconds, so 10s is plenty.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return ctx.Err()
	}
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
			mcp.WithDescription("Search emails using Gmail-like query syntax. Supports from:, to:, subject:, label:, has:attachment, before:, after:, and free text. "+
				"Paginate with offset/limit (default limit 20, max 50). Response: data, total, returned, offset, has_more. "+
				"(This server is not configured for vector search; only keyword FTS is available.)"),
			mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithDescription("Search emails using Gmail-like query syntax. Supports from:, to:, subject:, label:, has:attachment, before:, after:, and free text. "+
			"Returns compact summaries by default; set full=true for snippets, labels, and other extra fields. "+
			"All modes paginate via offset/limit (default limit 20, max 50). Response: data, total, returned, offset, has_more. "+
			"total=-1 means the full match count is unknown — use has_more. "+
			"Vector/hybrid ranking depth is capped by max_page_size_hybrid in config; beyond that use mode=fts. "+
			"Vector search is configured: set mode=vector for pure semantic search or mode=hybrid to fuse BM25 and vector ranking via RRF. Vector/hybrid modes require free-text terms in the query; filter-only queries must use mode=fts."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Gmail-style search query (e.g. 'from:alice subject:meeting after:2024-01-01'); mode=vector|hybrid require at least one free-text term"),
		),
		withAccount(),
		mcp.WithBoolean("full",
			mcp.Description("Return full message summaries instead of compact results (default false)"),
		),
		withLimit("20"),
		mcp.WithNumber("offset",
			mcp.Description("Number of results to skip for pagination (default 0)."),
		),
		mcp.WithString("mode",
			mcp.Description("Search mode: fts (default, keyword only), vector (semantic only), or hybrid (BM25 + vector fused via RRF)"),
			mcp.Enum(searchModeFTS, searchModeVector, searchModeHybrid),
		),
		mcp.WithBoolean("explain",
			mcp.Description("Include per-signal scores in the response (for debugging or ranking inspection)"),
		),
	)
}

func getMessageTool() mcp.Tool {
	return mcp.NewTool(ToolGetMessage,
		mcp.WithDescription("Get full message details including body text, recipients, labels, and attachments by message ID."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithNumber("id",
			mcp.Required(),
			mcp.Description("Message ID"),
		),
	)
}

func getAttachmentTool() mcp.Tool {
	return mcp.NewTool(ToolGetAttachment,
		mcp.WithDescription("Get attachment content by attachment ID. Returns metadata as text and the file content as an embedded resource blob. Use get_message first to find attachment IDs."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithNumber("attachment_id",
			mcp.Required(),
			mcp.Description("Attachment ID (from get_message response)"),
		),
	)
}

func exportAttachmentTool() mcp.Tool {
	return mcp.NewTool(ToolExportAttachment,
		mcp.WithDescription("Save an attachment to the local filesystem. Use this for file types that cannot be displayed inline (e.g. PDFs, documents). Returns the saved file path."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithDescription("List messages with optional filters. Returns compact summaries sorted by date by default; set full=true for snippets, labels, and other extra fields. "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, total, returned, offset, has_more. "+
			"total=-1 because the full count is not computed; use has_more for paging."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithBoolean("full",
			mcp.Description("Return full message summaries instead of compact results (default false)"),
		),
		withLimit("20"),
		withOffset(),
	)
}

func getStatsTool() mcp.Tool {
	return mcp.NewTool(ToolGetStats,
		mcp.WithDescription("Get archive overview: total messages, size, attachment count, and accounts."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
	)
}

func aggregateTool() mcp.Tool {
	return mcp.NewTool(ToolAggregate,
		mcp.WithDescription("Get grouped statistics (e.g. top senders, domains, labels, or message volume over time)."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
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

func searchByDomainsTool() mcp.Tool {
	return mcp.NewTool(ToolSearchByDomains,
		mcp.WithDescription("Find emails where any participant (from, to, or cc) belongs to one of the given domains. Useful for finding all communication with a company regardless of direction."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("domains",
			mcp.Required(),
			mcp.Description("Comma-separated domain names (e.g. 'gobright.com,ascentae.com')"),
		),
		withLimit("100"),
		withOffset(),
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
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		withAccount(),
		mcp.WithString("draft_id",
			mcp.Required(),
			mcp.Description("Draft ID (from list_drafts response)"),
		),
	)
}

func createDraftTool() mcp.Tool {
	return mcp.NewTool(ToolCreateDraft,
		mcp.WithDescription("Create a new email draft in Gmail. The draft is saved but NOT sent. Use send_draft to send it. For rich formatting (links, bold, etc.), set content_type to text/html and provide HTML body. To reply to an existing message, set thread_id AND in_reply_to (the original Message-ID) so the draft threads as a true reply in every mail client, not just Gmail."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithString("in_reply_to",
			mcp.Description("RFC 5322 Message-ID of the message being replied to, including angle brackets (e.g. '<abc@mail.gmail.com>'). Sets the In-Reply-To header so the draft is a true reply that nests correctly in any mail client (not just Gmail). Get this from the message's raw headers."),
		),
		mcp.WithString("references",
			mcp.Description("Space-separated Message-ID reference chain for the reply (the original References header plus the message being replied to). Defaults to in_reply_to when omitted."),
		),
		mcp.WithString("attachment_ids",
			mcp.Description("Attachment IDs to attach, comma-separated (from get_message). Attaches the archived file directly — no need to export first."),
		),
	)
}

func updateDraftTool() mcp.Tool {
	return mcp.NewTool(ToolUpdateDraft,
		mcp.WithDescription("Update an existing Gmail draft. Replaces the entire draft content."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithString("in_reply_to",
			mcp.Description("RFC 5322 Message-ID of the message being replied to, including angle brackets. Sets the In-Reply-To header for true reply threading across mail clients."),
		),
		mcp.WithString("references",
			mcp.Description("Space-separated Message-ID reference chain. Defaults to in_reply_to when omitted."),
		),
		mcp.WithString("attachment_ids",
			mcp.Description("Attachment IDs to attach, comma-separated (from get_message). Replaces any existing attachments."),
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
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(false),
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
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		withAccount(),
	)
}

func whatsAppStatusTool() mcp.Tool {
	return mcp.NewTool(ToolWhatsAppStatus,
		mcp.WithDescription("Get live WhatsApp connection and pairing status."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true), mcp.WithOpenWorldHintAnnotation(false),
		withAccount(),
	)
}

func whatsAppStartLoginTool() mcp.Tool {
	return mcp.NewTool(ToolWhatsAppStartLogin,
		mcp.WithDescription("Start or resume WhatsApp QR login for the live account. Returns status, QR payload, optional PNG bytes, and the browser QR page URL when configured."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true), mcp.WithOpenWorldHintAnnotation(true),
		withAccount(),
		mcp.WithNumber("wait_ms",
			mcp.Description("Milliseconds to wait for a QR code after starting login (default 3000, max 15000)"),
		),
		mcp.WithBoolean("include_qr_png",
			mcp.Description("Include base64 PNG QR image data when a QR code is available (default true)"),
		),
	)
}

func whatsAppLoginStatusTool() mcp.Tool {
	return mcp.NewTool(ToolWhatsAppLoginStatus,
		mcp.WithDescription("Poll WhatsApp QR login state for the live account. Returns the current QR payload, optional PNG bytes, and browser QR page URL when configured."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true), mcp.WithOpenWorldHintAnnotation(false),
		withAccount(),
		mcp.WithBoolean("include_qr_png",
			mcp.Description("Include base64 PNG QR image data when a QR code is available (default true)"),
		),
	)
}

func whatsAppLogoutTool() mcp.Tool {
	return mcp.NewTool(ToolWhatsAppLogout,
		mcp.WithDescription("Log out and unlink the live WhatsApp account, clearing local pairing state so it can be paired again. Destructive: requires confirm=true. By default, local session state is cleared even if the remote WhatsApp logout cannot complete."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false), mcp.WithOpenWorldHintAnnotation(true),
		withAccount(),
		mcp.WithBoolean("confirm",
			mcp.Required(),
			mcp.Description("Must be true to confirm logging out and clearing local WhatsApp pairing state."),
		),
		mcp.WithBoolean("force_local",
			mcp.Description("Clear local session state if the remote WhatsApp logout request fails (default true)."),
		),
	)
}

func sendWhatsAppMessageTool() mcp.Tool {
	return mcp.NewTool(ToolSendWhatsAppMessage,
		mcp.WithDescription("Send a WhatsApp message through the linked live account. Records an outbox row before sending and archives the sent message."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false), mcp.WithOpenWorldHintAnnotation(true),
		withAccount(),
		mcp.WithString("chat_id",
			mcp.Required(),
			mcp.Description("WhatsApp chat JID, group JID, or international phone number"),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("Message body to send"),
		),
		mcp.WithString("local_request_id",
			mcp.Description("Optional caller-provided idempotency/audit key"),
		),
	)
}

func sendWhatsAppReactionTool() mcp.Tool {
	return mcp.NewTool(ToolSendWhatsAppReaction,
		mcp.WithDescription("Set or clear a WhatsApp emoji reaction on an archived WhatsApp message. Use an empty emoji string to clear the active reaction."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false), mcp.WithOpenWorldHintAnnotation(true),
		withAccount(),
		mcp.WithNumber("message_id",
			mcp.Required(),
			mcp.Description("Archived msgvault message ID to react to"),
		),
		mcp.WithString("emoji",
			mcp.Required(),
			mcp.Description("Emoji reaction; pass an empty string to clear"),
		),
		mcp.WithString("local_request_id",
			mcp.Description("Optional caller-provided idempotency/audit key"),
		),
	)
}

func listGoogleDocsTool() mcp.Tool {
	return mcp.NewTool(ToolListGoogleDocs,
		mcp.WithDescription("List Google Docs in a configured Drive folder. Optionally filters by Drive name/fullText query."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true), mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithString("source",
			mcp.Description("Configured google_docs source name. Required when more than one source is configured."),
		),
		mcp.WithString("query",
			mcp.Description("Optional Drive search query matched against document name or full text"),
		),
		withLimit("20"),
	)
}

func searchGoogleDocsTool() mcp.Tool {
	return mcp.NewTool(ToolSearchGoogleDocs,
		mcp.WithDescription("Search Google Docs in a configured Drive folder and return matching document snippets for LLM context."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true), mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithString("source",
			mcp.Description("Configured google_docs source name. Required when more than one source is configured."),
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Text to search for in document name or full text"),
		),
		mcp.WithNumber("snippet_chars",
			mcp.Description("Maximum characters per snippet (default 1000, max 4000)"),
		),
		withLimit("10"),
	)
}

func getGoogleDocTool() mcp.Tool {
	return mcp.NewTool(ToolGetGoogleDoc,
		mcp.WithDescription("Export a Google Doc from a configured Drive folder as plain text."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true), mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithString("source",
			mcp.Description("Configured google_docs source name. Required when more than one source is configured."),
		),
		mcp.WithString("document_id",
			mcp.Required(),
			mcp.Description("Google Docs document ID"),
		),
		mcp.WithNumber("max_chars",
			mcp.Description("Maximum document text characters to return (default 20000, max 100000)"),
		),
	)
}

func appendGoogleDocTextTool() mcp.Tool {
	return mcp.NewTool(ToolAppendGoogleDocText,
		mcp.WithDescription("Append plain text to a Google Doc in a configured Drive folder."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false), mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithString("source",
			mcp.Description("Configured google_docs source name. Required when more than one source is configured."),
		),
		mcp.WithString("document_id",
			mcp.Required(),
			mcp.Description("Google Docs document ID"),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("Plain text to append"),
		),
	)
}

func replaceGoogleDocTextTool() mcp.Tool {
	return mcp.NewTool(ToolReplaceGoogleDocText,
		mcp.WithDescription("Replace all matching text in a Google Doc in a configured Drive folder."),
		mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false), mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithString("source",
			mcp.Description("Configured google_docs source name. Required when more than one source is configured."),
		),
		mcp.WithString("document_id",
			mcp.Required(),
			mcp.Description("Google Docs document ID"),
		),
		mcp.WithString("find",
			mcp.Required(),
			mcp.Description("Substring to find"),
		),
		mcp.WithString("replacement",
			mcp.Required(),
			mcp.Description("Replacement text. Use an empty string to delete matched text."),
		),
		mcp.WithBoolean("match_case",
			mcp.Description("Whether the search should be case sensitive (default false)"),
		),
	)
}

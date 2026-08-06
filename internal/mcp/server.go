package mcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
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
	ToolSearchMessages         = "search_messages"
	ToolSearchMetadata         = "search_metadata"
	ToolSearchMessageBodies    = "search_message_bodies"
	ToolSemanticSearchMessages = "semantic_search_messages"
	ToolGetMessage             = "get_message"
	ToolGetAttachment          = "get_attachment"
	ToolExportAttachment       = "export_attachment"
	ToolListMessages           = "list_messages"
	ToolGetStats               = "get_stats"
	ToolAggregate              = "aggregate"
	ToolStageDeletion          = "stage_deletion"
	ToolSearchByDomains        = "search_by_domains"
	ToolFindSimilarMessages    = "find_similar_messages"
	ToolSearchInMessage        = "search_in_message"
	ToolListDrafts             = "list_drafts"
	ToolGetDraft               = "get_draft"
	ToolCreateDraft            = "create_draft"
	ToolUpdateDraft            = "update_draft"
	ToolDeleteDraft            = "delete_draft"
	ToolSendDraft              = "send_draft"
	ToolModifyLabels           = "modify_labels"
	ToolCreateLabel            = "create_label"
	ToolDeleteLabel            = "delete_label"
	ToolListGmailLabels        = "list_gmail_labels"
	ToolWhatsAppStatus         = "whatsapp_status"
	ToolWhatsAppStartLogin     = "whatsapp_start_login"
	ToolWhatsAppLoginStatus    = "whatsapp_login_status"
	ToolWhatsAppLogout         = "whatsapp_logout"
	ToolSendWhatsAppMessage    = "send_whatsapp_message"
	ToolSendWhatsAppReaction   = "send_whatsapp_reaction"
	ToolListGoogleDocs         = "list_google_docs"
	ToolSearchGoogleDocs       = "search_google_docs"
	ToolGetGoogleDoc           = "get_google_doc"
	ToolAppendGoogleDocText    = "append_google_doc_text"
	ToolReplaceGoogleDocText   = "replace_google_doc_text"
)

// search_message_bodies/search_in_message mode values (wire format).
const (
	searchModeKeyword = "keyword"
	searchModeVector  = "vector"
	searchModeHybrid  = "hybrid"
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
// the search_message_bodies tool, and Backend additionally enables the
// find_similar_messages tool.
type ServeOptions struct {
	Engine           query.Engine
	AttachmentsDir   string
	AttachmentReader AttachmentReader
	ManifestSaver    DeletionManifestSaver
	HybridSearcher   HybridSearcher
	SimilarSearcher  SimilarSearcher
	DataDir          string

	// HybridEngine is optional. When nil, semantic_search_messages rejects
	// vector/hybrid searches with a vector_not_enabled error.
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
		attachmentReader:  opts.AttachmentReader,
		manifestSaver:     opts.ManifestSaver,
		hybridSearcher:    opts.HybridSearcher,
		similarSearcher:   opts.SimilarSearcher,
		dataDir:           opts.DataDir,
		hybridEngine:      opts.HybridEngine,
		vectorCfg:         opts.VectorCfg,
		backend:           opts.Backend,
		gmailFactory:      opts.GmailFactory,
		whatsAppFactory:   opts.WhatsAppFactory,
		whatsAppLoginURL:  strings.TrimSpace(opts.WhatsAppLoginURL),
		googleDocsFactory: opts.GoogleDocsFactory,
	}

	vectorAvailable := opts.HybridEngine != nil || opts.HybridSearcher != nil
	// search_in_message mode=vector needs the in-process vector components
	// (HybridEngine + Backend as ChunkScoringBackend), not just the daemon
	// HybridSearcher. The production CLI only wires the daemon searcher, so
	// gate the vector mode advertisement on the actual capability.
	vectorInMessageAvailable := opts.HybridEngine != nil && opts.Backend != nil
	s.AddTool(searchMessagesTool(vectorAvailable), h.searchMessages)
	s.AddTool(searchMetadataTool(), h.searchMetadata)
	s.AddTool(searchMessageBodiesTool(), h.searchMessageBodies)
	s.AddTool(semanticSearchMessagesTool(vectorAvailable), h.semanticSearchMessages)
	s.AddTool(getMessageTool(), h.getMessage)
	s.AddTool(getAttachmentTool(), h.getAttachment)
	s.AddTool(searchInMessageTool(vectorInMessageAvailable), h.searchInMessage)
	s.AddTool(exportAttachmentTool(), h.exportAttachment)
	s.AddTool(listMessagesTool(), h.listMessages)
	s.AddTool(getStatsTool(), h.getStats)
	s.AddTool(aggregateTool(), h.aggregate)
	s.AddTool(stageDeletionTool(), h.stageDeletion)
	s.AddTool(searchByDomainsTool(), h.searchByDomains)
	if opts.Backend != nil || opts.SimilarSearcher != nil {
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
func ServeHTTPWithOptions(ctx context.Context, opts ServeOptions, addr, apiKey string) error {
	stdlibServer := newMCPHTTPServer(opts, addr, apiKey)
	fmt.Fprintf(os.Stderr, "Starting MCP server on %s\n", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := stdlibServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		_ = stdlibServer.Shutdown(shutdownCtx)
		return ctx.Err()
	}
}

func newMCPHTTPServer(opts ServeOptions, addr, apiKey string) *http.Server {
	stdlibServer := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpServer := server.NewStreamableHTTPServer(
		BuildMCPServer(opts),
		server.WithStreamableHTTPServer(stdlibServer),
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", bearerAuthHandler(apiKey, httpServer))
	stdlibServer.Handler = mux
	return stdlibServer
}

func bearerAuthHandler(apiKey string, next http.Handler) http.Handler {
	if apiKey == "" {
		return next
	}

	expected := sha256.Sum256([]byte(apiKey))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		authorized := false
		if len(values) == 1 {
			scheme, credential, found := strings.Cut(values[0], " ")
			if found && credential != "" && strings.EqualFold(scheme, "Bearer") {
				supplied := sha256.Sum256([]byte(credential))
				authorized = subtle.ConstantTimeCompare(expected[:], supplied[:]) == 1
			}
		}

		if !authorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Shared search_metadata schema text. The parser implements a subset of Gmail
// syntax — not full Gmail compatibility. Keep this in sync with
// internal/search/parser.go and the SearchFast path in handlers.go.
const (
	searchMetadataOperatorDoc = "Supported operators: from:, to:, cc:, bcc:, subject:, label: (or l:), has:attachment, " +
		"before:/after: (YYYY-MM-DD), older_than:/newer_than: (e.g. 7d, 2w, 1m, 1y), larger:/smaller: (e.g. 5M). " +
		"Bare domains on from:/to: match any address at that domain. Multiple terms are ANDed. " +
		"Not supported: negation (-), OR, or parentheses grouping."
	searchMetadataFreeTextDoc = "Free text matches subject, snippet, and sender/recipient metadata only (not bodies). " +
		"Use search_message_bodies for body keywords or semantic_search_messages for vector/hybrid search."
	searchMetadataPaginationDoc = "Results are ordered newest-first (by sent date); there is no sort parameter — " +
		"use before:/after: to scope a date range. " +
		"Paginate with offset/limit (default limit 20, max 50). " +
		"Response: data, total, returned, offset, has_more."
)

func searchMetadataTool() mcp.Tool {
	searchIntro := "Search message metadata using a subset of Gmail query syntax (not full Gmail compatibility). " +
		searchMetadataOperatorDoc + " " + searchMetadataFreeTextDoc + " "
	queryDesc := "Search query (e.g. 'from:alice subject:meeting after:2024-01-01'). " +
		"See tool description for supported operators and limitations."

	return mcp.NewTool(ToolSearchMetadata,
		mcp.WithDescription(searchIntro+searchMetadataPaginationDoc+
			"For body keywords use search_message_bodies; for vector/hybrid search use semantic_search_messages."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description(queryDesc),
		),
		withAccount(),
		mcp.WithBoolean("full",
			mcp.Description("Return full message summaries instead of compact results (default false)"),
		),
		withLimit("20"),
		withOffset(),
	)
}

// searchMessagesTool preserves the pre-split search_messages contract for
// existing MCP clients. New clients should use search_metadata for metadata
// queries and semantic_search_messages for vector/hybrid queries.
func searchMessagesTool(vectorAvailable bool) mcp.Tool {
	description := "Deprecated compatibility tool; use search_metadata when mode is omitted and semantic_search_messages for mode=vector or mode=hybrid. " +
		searchMetadataOperatorDoc + " " + searchMetadataFreeTextDoc + " " + searchMetadataPaginationDoc
	opts := []mcp.ToolOption{
		mcp.WithDescription(description),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query; omit mode for metadata search or set mode=vector|hybrid for semantic search"),
		),
		withAccount(),
		withLimit("20"),
		withOffset(),
	}
	if vectorAvailable {
		opts = append(opts,
			mcp.WithString("mode",
				mcp.Description("Search mode: vector or hybrid. Omit for metadata search."),
				mcp.Enum(searchModeVector, searchModeHybrid),
			),
			mcp.WithBoolean("explain",
				mcp.Description("Include per-signal scores for vector/hybrid results"),
			),
			mcp.WithNumber("min_score",
				mcp.Description("Minimum semantic score for returned chunk excerpts; does not filter ranked messages"),
			),
		)
	}
	return mcp.NewTool(ToolSearchMessages, opts...)
}

// searchMessageBodiesTool is the keyword-only body search. It is deliberately
// separate from semanticSearchMessagesTool: keyword results are term-delimited
// (a finite set of messages containing the query), date-ordered, and can report
// an exact total, whereas semantic results are threshold-delimited (unbounded,
// score-ordered, no total). Splitting the tools keeps each contract honest and
// removes the mode/explain/min_score params that only ever applied to semantic.
func searchMessageBodiesTool() mcp.Tool {
	searchIntro := "Keyword full-text search over message bodies. " +
		"Returns messages whose body text contains the query terms, newest-first, " +
		"each with matches — up to 5 excerpt snippets centered on matched terms. " +
		"Backend excerpts may omit char_offset and line when efficient source locations are unavailable; use search_in_message when exact locations are needed. " +
		"When matches_truncated is true on a hit, more than 5 excerpts matched — use search_in_message or get_message to read the full body. " +
		"Known Gmail operators (from:, subject:, label:, etc.) apply as metadata filters only and do not satisfy the free-text requirement. " +
		"Filter-only queries such as from:alice are rejected — use search_metadata for filter-only queries. " +
		"Unrecognized word:value tokens (e.g. RXD2:V2) are treated as literal body text, not filters. " +
		"Query syntax: space-separated words are ANDed (each must appear somewhere in the body); " +
		"a double-quoted phrase is one exact phrase (e.g. \"RXD2 V2\"); OR and NOT are not supported. " +
		searchMetadataOperatorDoc + " "
	queryDesc := "Body search query with at least one free-text term (bare word or quoted phrase). " +
		"Gmail operators (from:, subject:, etc.) are metadata filters, not body search — " +
		"subject:test alone is rejected; combine with body terms (from:alice budget) or use search_metadata for filter-only queries. " +
		"Unrecognized word:value tokens (RXD2:V2) are literal text. " +
		"Space-separated words are ANDed; double quotes match an exact phrase; OR/NOT unsupported."

	return mcp.NewTool(ToolSearchMessageBodies,
		mcp.WithDescription(searchIntro+
			"Results are ordered newest-first (by sent date). "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, returned, offset, has_more. "+
			"Body search does not return a total; use has_more to detect more pages."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description(queryDesc),
		),
		withAccount(),
		withLimit("20"),
		withOffset(),
	)
}

// semanticSearchMessagesTool is the vector/hybrid body search. See the note on
// searchMessageBodiesTool: this tool owns the score-ordered, unbounded contract
// (mode/explain/min_score, and the mode/pool_saturated/generation response fields)
// that would be dead weight on the keyword tool.
func semanticSearchMessagesTool(vectorAvailable bool) mcp.Tool {
	if !vectorAvailable {
		return mcp.NewTool(ToolSemanticSearchMessages,
			mcp.WithDescription("Semantic (embedding) search over message bodies is unavailable: "+
				"vector search is not configured on this server."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Free-text query to embed (requires at least one free-text term)"),
			),
		)
	}
	searchIntro := "Semantic (embedding) search over each preprocessed message subject and body. " +
		"Returns messages ranked by similarity to the query — there is no exact total, so page on has_more. " +
		"Each hit includes matches — embedded subject/body chunks ranked by semantic similarity (up to 5 per message), each with a score. " +
		"Vector char_offset and line locations may be omitted because preprocessing usually prevents exact raw-body mapping; use snippet terms with search_in_message keyword mode when navigation is needed. " +
		"min_score filters chunk excerpts only; it does not remove or reorder ranked messages. " +
		"Requires at least one free-text term (used to embed); filter-only queries must use search_metadata. " +
		"Known Gmail operators (from:, subject:, label:, etc.) apply as metadata filters only. " +
		searchMetadataOperatorDoc + " "
	queryDesc := "Free-text query to embed (requires at least one free-text term). " +
		"Gmail operators are metadata filters, not body search; combine with body terms or use search_metadata for filter-only queries."

	return mcp.NewTool(ToolSemanticSearchMessages,
		mcp.WithDescription(searchIntro+
			"mode=vector for pure semantic search or mode=hybrid to fuse BM25 and vector ranking via RRF. "+
			"Paginate with offset/limit (default limit 20, max 50). Response: data, returned, offset, has_more, mode, pool_saturated, generation. "+
			"total is not available; use has_more to page."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description(queryDesc),
		),
		withAccount(),
		withLimit("20"),
		withOffset(),
		mcp.WithString("mode",
			mcp.Description("Search mode: vector (semantic only) or hybrid (BM25 + vector fused via RRF). Defaults to hybrid when omitted."),
			mcp.Enum(searchModeVector, searchModeHybrid),
		),
		mcp.WithBoolean("explain",
			mcp.Description("Include per-signal scores in the response (for debugging or ranking inspection)"),
		),
		mcp.WithNumber("min_score",
			mcp.Description("Minimum chunk similarity score for included match excerpts (default 0); does not filter ranked messages"),
		),
	)
}

func getMessageTool() mcp.Tool {
	return mcp.NewTool(ToolGetMessage,
		mcp.WithDescription("Get message details including recipients, labels, attachments, and a slice of the message body. "+
			"Returns plain text when available; HTML-only messages return a body_html slice with body_format=html. "+
			"Body paging mirrors search pagination: body_length=total bytes, offset=where this chunk starts, body_returned=bytes in this chunk, has_more=more body follows. "+
			"To read sequentially: call again with offset += body_returned. "+
			"To jump to a known match location: use center_at=<byte offset> to center the window on that location. "+
			"Note: snippet is pre-stored source metadata (may be empty for non-Gmail sources)."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithNumber("id",
			mcp.Required(),
			mcp.Description("Message ID"),
		),
		mcp.WithNumber("offset",
			mcp.Description("Byte offset from the start of the selected body to begin reading (default 0). Ignored when center_at is provided."),
		),
		mcp.WithNumber("center_at",
			mcp.Description("Byte offset from the start of the selected body to center the window on. Takes precedence over offset."),
		),
		mcp.WithNumber("max_chars",
			mcp.Description("Maximum selected-body bytes to return (default 2000, max 4000). Values above 4000 are clamped to 4000; zero or negative values use the default."),
		),
		mcp.WithString("body_format",
			mcp.Description("Which body representation to page: auto (default, plain text when available, HTML fallback), text, or html."),
			mcp.Enum(bodyFormatAuto, bodyFormatText, bodyFormatHTML),
		),
		mcp.WithBoolean("full_body",
			mcp.Description("Return the complete selected body in one response, ignoring offset, center_at, and max_chars. Use only when the full content is explicitly needed."),
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

func searchInMessageTool(vectorInMessageAvailable bool) mcp.Tool {
	desc := "Find matches within one message body. Default mode=keyword finds literal term occurrences. " +
		"Each match includes char_offset (byte offset into body_text), snippet, and line. " +
		"Use char_offset with get_message center_at to read a larger window around any match."
	if vectorInMessageAvailable {
		desc = "Find matches within one message body. Default mode=keyword finds literal term occurrences. " +
			"mode=vector scores each embedded chunk by semantic similarity to the query (best first, with score on each match). " +
			"Keyword matches include raw-body char_offset and line. Vector matches always include snippet and score; char_offset and line may be omitted after preprocessing. " +
			"Use a present char_offset with get_message center_at to read a larger window around the match."
	}

	opts := []mcp.ToolOption{
		mcp.WithDescription(desc),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithNumber("id",
			mcp.Required(),
			mcp.Description("Message ID"),
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query (keyword term, or semantic query when mode=vector)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum matches to return (default 10)"),
		),
		withOffset(),
	}
	if vectorInMessageAvailable {
		opts = append(opts,
			mcp.WithString("mode",
				mcp.Description("Search mode: keyword (default, literal term) or vector (semantic chunk scoring)"),
				mcp.Enum(searchModeKeyword, searchModeVector),
			),
			mcp.WithNumber("min_score",
				mcp.Description("Minimum chunk similarity score (0–1) when mode=vector (default 0)"),
			),
		)
	}
	return mcp.NewTool(ToolSearchInMessage, opts...)
}

func listMessagesTool() mcp.Tool {
	return mcp.NewTool(ToolListMessages,
		mcp.WithDescription("List messages with optional filters, newest-first. "+
			"Returns compact summaries by default; set full=true for snippets, labels, and other extra fields. "+
			"Pass conversation_id to enumerate a thread's messages, then call get_message(id) per message to read bodies — "+
			"there is deliberately no bulk body fetch, to avoid loading huge threads into the context window. "+
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
		mcp.WithNumber("conversation_id",
			mcp.Description("Filter by conversation/thread ID"),
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
		mcp.WithDescription("Get grouped statistics (top senders, recipients, domains, labels, or message volume by calendar year). "+
			"Returns a JSON array of objects with fields Key, Count, TotalSize, AttachmentSize, AttachmentCount, and TotalUnique."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("group_by",
			mcp.Required(),
			mcp.Description("Dimension to group by. When 'time', buckets are by calendar year only (Key is a year string like \"2024\")."),
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
		mcp.WithString("message_type",
			mcp.Description("Restrict results to one message type, such as email, sms, mms, fbmessenger, or calendar_event"),
		),
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
		mcp.WithDescription("Get live WhatsApp connection and pairing status. WhatsApp is usable only when ready=true, meaning paired, connected, and logged_in are all true."),
		mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true), mcp.WithOpenWorldHintAnnotation(false),
		withAccount(),
	)
}

func whatsAppStartLoginTool() mcp.Tool {
	return mcp.NewTool(ToolWhatsAppStartLogin,
		mcp.WithDescription("Start or resume WhatsApp QR login for the live account. Returns status, ready, QR payload, optional PNG bytes, and the browser QR page URL when configured. Continue polling until ready=true before sending messages."),
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
		mcp.WithDescription("Poll WhatsApp QR login state for the live account. Returns ready, current QR payload, optional PNG bytes, and browser QR page URL when configured. Continue polling until ready=true before sending messages."),
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
		mcp.WithDescription("Send a WhatsApp message through the linked live account. Requires whatsapp_status ready=true. Records an outbox row before sending and archives the sent message."),
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
		mcp.WithArray("mentions",
			mcp.Description("Optional @mentions: full JID strings (e.g. \"178357123686403@lid\" or \"33612345678@s.whatsapp.net\"). The body must contain a matching \"@<user>\" token per JID (the digits before @), which WhatsApp renders as the contact's name and pings. Mainly for group messages."),
			mcp.Items(map[string]any{"type": "string"}),
		),
		mcp.WithString("local_request_id",
			mcp.Description("Optional caller-provided idempotency/audit key"),
		),
	)
}

func sendWhatsAppReactionTool() mcp.Tool {
	return mcp.NewTool(ToolSendWhatsAppReaction,
		mcp.WithDescription("Set or clear a WhatsApp emoji reaction on an archived WhatsApp message. Requires whatsapp_status ready=true. Use an empty emoji string to clear the active reaction."),
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

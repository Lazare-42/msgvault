package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/googledocs"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

const (
	maxLimit                      = 1000
	maxSearchMessagesLimit        = 50
	defaultSearchLimit            = 20
	defaultGoogleDocsListLimit    = 20
	defaultGoogleDocsSearchLimit  = 10
	defaultGoogleDocsSnippetChars = 1000
	defaultGoogleDocsMaxChars     = 20000
	maxGoogleDocsListLimit        = 100
	maxGoogleDocsSearchLimit      = 20
	maxGoogleDocsSnippetChars     = 4000
	maxGoogleDocsMaxChars         = 100000
	// totalCountUnknown is returned when the backend cannot report a full match
	// count (body FTS fallback, hybrid/vector ranking depth, or list_messages
	// without a separate count query). Clients should use has_more for paging.
	totalCountUnknown = -1
)

type paginatedResponse[T any] struct {
	Data     []T   `json:"data"`
	Total    int64 `json:"total"`
	Returned int   `json:"returned"`
	Offset   int   `json:"offset"`
	HasMore  bool  `json:"has_more"`
}

func newPaginatedResponse[T any](data []T, total int64, offset int) paginatedResponse[T] {
	if data == nil {
		data = []T{}
	}
	returned := len(data)
	return paginatedResponse[T]{
		Data:     data,
		Total:    total,
		Returned: returned,
		Offset:   offset,
		HasMore:  int64(offset+returned) < total,
	}
}

// newPaginatedResponseNoTotal builds a page when the backend cannot report a
// total match count. total is always totalCountUnknown; use has_more to page.
func newPaginatedResponseNoTotal[T any](data []T, offset int, hasMore bool) paginatedResponse[T] {
	if data == nil {
		data = []T{}
	}
	return paginatedResponse[T]{
		Data:     data,
		Total:    totalCountUnknown,
		Returned: len(data),
		Offset:   offset,
		HasMore:  hasMore,
	}
}

func searchLimitArg(args map[string]any) int {
	limit := limitArg(args, "limit", defaultSearchLimit)
	if limit <= 0 {
		return defaultSearchLimit
	}
	if limit > maxSearchMessagesLimit {
		return maxSearchMessagesLimit
	}
	return limit
}

func listLimitArg(args map[string]any) int {
	return searchLimitArg(args)
}

type handlers struct {
	engine            query.Engine
	attachmentsDir    string
	dataDir           string
	gmailFactory      GmailClientFactory
	whatsAppFactory   WhatsAppClientFactory
	googleDocsFactory GoogleDocsClientFactory

	// Optional vector-search wiring. When hybridEngine is nil, the
	// search_messages handler rejects mode=vector and mode=hybrid with
	// a vector_not_enabled error. backend is additionally required by
	// the find_similar_messages handler to load seed vectors and
	// resolve the active generation.
	hybridEngine *hybrid.Engine
	vectorCfg    vector.Config
	backend      vector.Backend
}

// translateVectorErr maps well-known vector sentinel errors to MCP tool
// error results. Returns nil if the error is not a known sentinel
// (callers should wrap it themselves).
func translateVectorErr(err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, vector.ErrNotEnabled):
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured",
		)
	case errors.Is(err, vector.ErrIndexStale):
		return mcp.NewToolResultError(
			"index_stale: the vector index does not match the configured model; " +
				"run `msgvault embeddings build --full-rebuild`",
		)
	case errors.Is(err, vector.ErrIndexBuilding):
		return mcp.NewToolResultError(
			"index_building: the initial vector index is still being built",
		)
	case errors.Is(err, vector.ErrNoActiveGeneration):
		return mcp.NewToolResultError(
			"no_active_generation: vector search has no active index yet; " +
				"run `msgvault embeddings build` to build one",
		)
	case errors.Is(err, vector.ErrEmbeddingTimeout):
		return mcp.NewToolResultError(
			"embedding_timeout: the embedding endpoint did not respond in time; " +
				"retry, or raise [vector.embeddings].timeout in config",
		)
	}
	return nil
}

type messageSummaryCompact struct {
	ID                   int64     `json:"id"`
	SourceMessageID      string    `json:"source_message_id"`
	ConversationID       int64     `json:"conversation_id"`
	SourceConversationID string    `json:"source_conversation_id"`
	Subject              string    `json:"subject"`
	FromEmail            string    `json:"from_email"`
	FromName             string    `json:"from_name"`
	SentAt               time.Time `json:"sent_at"`
	HasAttachments       bool      `json:"has_attachments"`
}

func compactMessageSummaries(results []query.MessageSummary) []messageSummaryCompact {
	out := make([]messageSummaryCompact, len(results))
	for i, msg := range results {
		out[i] = messageSummaryCompact{
			ID:                   msg.ID,
			SourceMessageID:      msg.SourceMessageID,
			ConversationID:       msg.ConversationID,
			SourceConversationID: msg.SourceConversationID,
			Subject:              msg.Subject,
			FromEmail:            msg.FromEmail,
			FromName:             msg.FromName,
			SentAt:               msg.SentAt,
			HasAttachments:       msg.HasAttachments,
		}
	}
	return out
}

// getAccountID looks up a source ID by email address.
// Returns nil if account is empty (no filter), or an error if not found.
func (h *handlers) getAccountID(ctx context.Context, account string) (*int64, error) {
	if account == "" {
		return nil, nil //nolint:nilnil // empty input -> no filter, not an error
	}
	accounts, err := h.engine.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts: %w", err)
	}
	for _, acc := range accounts {
		if acc.Identifier == account {
			return &acc.ID, nil
		}
	}
	return nil, fmt.Errorf("account not found: %s", account)
}

// getIDArg extracts a required positive integer ID from the arguments map.
func getIDArg(args map[string]any, key string) (int64, error) {
	v, ok := args[key].(float64)
	if !ok {
		return 0, fmt.Errorf("%s parameter is required", key)
	}
	if v != math.Trunc(v) || v < 1 || v > math.MaxInt64 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return int64(v), nil
}

// getDateArg extracts an optional date (YYYY-MM-DD) from the arguments map.
func getDateArg(args map[string]any, key string) (*time.Time, error) {
	v, ok := args[key].(string)
	if !ok || v == "" {
		return nil, nil //nolint:nilnil // absent optional arg is not an error
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return nil, fmt.Errorf("invalid %s date %q: expected YYYY-MM-DD", key, v)
	}
	return &t, nil
}

// readAttachmentFile reads the content-addressed attachment file after
// validating the hash and checking size limits.
func (h *handlers) readAttachmentFile(contentHash string) ([]byte, error) {
	filePath, err := export.StoragePath(h.attachmentsDir, contentHash)
	if err != nil {
		return nil, errors.New("attachment has invalid content hash")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	if info.Size() > maxAttachmentSize {
		return nil, fmt.Errorf("attachment too large: %d bytes (max %d)", info.Size(), maxAttachmentSize)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxAttachmentSize+1))
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	if int64(len(data)) > maxAttachmentSize {
		return nil, fmt.Errorf("attachment too large: %d bytes (max %d)", len(data), maxAttachmentSize)
	}

	return data, nil
}

// maxDraftAttachmentsSize caps the combined raw size of attachments on a single
// draft. Gmail rejects messages over ~25MB; base64 inflates content by ~33%, so
// the raw total must stay below that ceiling.
const maxDraftAttachmentsSize = 18 * 1024 * 1024

// resolveDraftAttachments loads the attachments named by the comma-separated
// "attachment_ids" argument from the archive into draft attachments. Returns
// (nil, nil) when no attachment_ids are provided.
func (h *handlers) resolveDraftAttachments(ctx context.Context, args map[string]any) ([]gmail.DraftAttachment, error) {
	raw, _ := args["attachment_ids"].(string)
	ids := splitCSV(raw)
	if len(ids) == 0 {
		return nil, nil
	}
	if h.attachmentsDir == "" {
		return nil, fmt.Errorf("attachments directory not configured")
	}

	var atts []gmail.DraftAttachment
	var total int64
	for _, s := range ids {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id < 1 {
			return nil, fmt.Errorf("invalid attachment_id %q: must be a positive integer", s)
		}
		att, err := h.engine.GetAttachment(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("get attachment %d: %v", id, err)
		}
		if att == nil {
			return nil, fmt.Errorf("attachment %d not found", id)
		}
		data, err := h.readAttachmentFile(att.ContentHash)
		if err != nil {
			return nil, fmt.Errorf("attachment %d: %v", id, err)
		}
		total += int64(len(data))
		if total > maxDraftAttachmentsSize {
			return nil, fmt.Errorf("total attachment size exceeds %d bytes", maxDraftAttachmentsSize)
		}
		atts = append(atts, gmail.DraftAttachment{
			Filename:    att.Filename,
			ContentType: att.MimeType,
			Content:     data,
		})
	}
	return atts, nil
}

func (h *handlers) searchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	queryStr, _ := args["query"].(string)
	if queryStr == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = searchModeFTS
	}
	explain, _ := args["explain"].(bool)

	if mode == searchModeVector || mode == searchModeHybrid {
		return h.searchMessagesHybrid(ctx, args, queryStr, mode, explain)
	}

	if mode != searchModeFTS {
		return mcp.NewToolResultError(
			fmt.Sprintf("invalid mode %q: must be %s, %s, or %s", mode, searchModeFTS, searchModeVector, searchModeHybrid),
		), nil
	}

	limit := searchLimitArg(args)
	offset := limitArg(args, "offset", 0)

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	q := search.Parse(queryStr)
	if sourceID != nil {
		q.AccountIDs = []int64{*sourceID}
	}

	filter := query.MessageFilter{SourceID: sourceID}

	// Try fast search first (metadata only), fall back to full FTS.
	results, err := h.engine.SearchFast(ctx, q, filter, limit, offset)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	// If fast search returns nothing and query has free text, try full FTS.
	if len(results) == 0 && len(q.TextTerms) > 0 {
		results, err = h.engine.Search(ctx, q, limit+1, offset)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}
		hasMore := len(results) > limit
		if hasMore {
			results = results[:limit]
		}
		return jsonResult(newPaginatedResponseNoTotal(results, offset, hasMore))
	}

	totalMatched, err := h.engine.SearchFastCount(ctx, q, filter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search count failed: %v", err)), nil
	}

	if full, _ := args["full"].(bool); full {
		return jsonResult(newPaginatedResponse(results, totalMatched, offset))
	}

	// Compact summaries are the default for MCP to keep agent context lean.
	// Call get_message for a chosen hit when the full body/labels/attachments matter.
	return jsonResult(newPaginatedResponse(compactMessageSummaries(results), totalMatched, offset))
}

// hybridScoreBreakdown exposes fused-score components for debugging.
// All score fields are pointer-typed so "not present in this signal"
// can be distinguished from a legitimate 0.0 score. RRF is omitted in
// mode=vector (only one signal, nothing to fuse).
type hybridScoreBreakdown struct {
	RRF            *float64 `json:"rrf,omitempty"`
	BM25           *float64 `json:"bm25,omitempty"`
	Vector         *float64 `json:"vector,omitempty"`
	SubjectBoosted bool     `json:"subject_boosted,omitempty"`
}

// hybridMessageItem is a single hit in a vector/hybrid response. The
// embedded MessageSummary carries the standard message fields; Score is
// present only when explain=true was requested.
type hybridMessageItem struct {
	query.MessageSummary

	Score *hybridScoreBreakdown `json:"score,omitempty"`
}

// hybridGenerationSummary describes the active vector-index generation
// used to answer a hybrid/vector query.
type hybridGenerationSummary struct {
	ID          int64  `json:"id"`
	Model       string `json:"model"`
	Dimension   int    `json:"dimension"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
}

// searchMessagesHybridResponse is the paginated body for mode=vector|hybrid.
type searchMessagesHybridResponse struct {
	paginatedResponse[hybridMessageItem]

	Mode          string                  `json:"mode"`
	PoolSaturated bool                    `json:"pool_saturated"`
	Generation    hybridGenerationSummary `json:"generation"`
}

// searchMessagesHybrid runs vector or hybrid search via the configured
// hybrid engine. Mirrors api/handlers.go handleHybridSearch: returns
// descriptive errors when the engine is not configured or the index is
// stale/building, otherwise returns RRF-ranked hits hydrated via
// engine.GetMessage.
func (h *handlers) searchMessagesHybrid(
	ctx context.Context, args map[string]any,
	queryStr, mode string, explain bool,
) (*mcp.CallToolResult, error) {
	if h.hybridEngine == nil {
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured on this server",
		), nil
	}

	// Resolve account filter to a source ID for the structured Filter.
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := searchLimitArg(args)
	offset := limitArg(args, "offset", 0)

	parsed := search.Parse(queryStr)
	freeText := strings.Join(parsed.TextTerms, " ")

	// mode=vector|hybrid requires at least one free-text term; filter-only
	// queries have no query vector to rank by. Callers that want pure
	// structured filtering should use mode=fts instead.
	if freeText == "" {
		return mcp.NewToolResultError(
			"missing_free_text: mode=" + mode +
				" requires at least one free-text term; use mode=fts for filter-only queries",
		), nil
	}

	subjectTerms := make([]string, 0, len(parsed.TextTerms))
	for _, t := range parsed.TextTerms {
		subjectTerms = append(subjectTerms, strings.ToLower(t))
	}

	filter, err := h.hybridEngine.BuildFilter(ctx, parsed)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("filter resolution failed: %v", err)), nil
	}
	if sourceID != nil {
		filter.SourceIDs = []int64{*sourceID}
	}

	maxPage := h.vectorCfg.Search.MaxPageSizeHybridClamp()
	requestedEnd := offset + limit
	wantedFetch := requestedEnd + 1 // probe one past the page end for has_more
	fetchLimit := wantedFetch
	hitMaxPageCap := false
	if maxPage > 0 {
		if offset >= maxPage {
			return mcp.NewToolResultError(fmt.Sprintf(
				"pagination_limit: offset %d exceeds hybrid ranking window (max %d); "+
					"use mode=fts for deeper pagination",
				offset, maxPage,
			)), nil
		}
		if fetchLimit > maxPage {
			fetchLimit = maxPage
			hitMaxPageCap = wantedFetch > maxPage
		}
	}

	req := hybrid.SearchRequest{
		Mode:         hybrid.Mode(mode),
		FreeText:     freeText,
		Filter:       filter,
		Limit:        fetchLimit,
		SubjectTerms: subjectTerms,
		Explain:      explain,
	}

	hits, meta, err := h.hybridEngine.Search(ctx, req)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	// Bulk-hydrate hits in one round-trip instead of looping
	// GetMessage per result (which fetches body, From, To, Cc, Bcc,
	// labels, and attachments for each id and was the dominant search
	// latency cost).
	hitIDs := make([]int64, len(hits))
	for i, h := range hits {
		hitIDs[i] = h.MessageID
	}
	summaries, err := h.engine.GetMessageSummariesByIDs(ctx, hitIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"mcp: hydrate hybrid hits failed: ids=%d error=%v\n",
			len(hitIDs), err)
		summaries = nil
	}
	byID := make(map[int64]query.MessageSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
	}
	items := make([]hybridMessageItem, 0, len(hits))
	for _, hit := range hits {
		msg, ok := byID[hit.MessageID]
		if !ok {
			continue
		}
		item := hybridMessageItem{MessageSummary: msg}
		if explain {
			sb := &hybridScoreBreakdown{SubjectBoosted: hit.SubjectBoosted}
			if !math.IsNaN(hit.RRFScore) {
				v := hit.RRFScore
				sb.RRF = &v
			}
			if !math.IsNaN(hit.BM25Score) {
				v := hit.BM25Score
				sb.BM25 = &v
			}
			if !math.IsNaN(hit.VectorScore) {
				v := hit.VectorScore
				sb.Vector = &v
			}
			item.Score = sb
		}
		items = append(items, item)
	}

	var page []hybridMessageItem
	if offset < len(items) {
		end := min(offset+limit, len(items))
		page = items[offset:end]
	}
	nextPageServable := maxPage == 0 || requestedEnd < maxPage
	hasMore := false
	if nextPageServable {
		if requestedEnd < len(items) {
			hasMore = true
		} else if !hitMaxPageCap && meta.PoolSaturated && len(hits) >= fetchLimit {
			hasMore = true
		}
	}

	return jsonResult(searchMessagesHybridResponse{
		paginatedResponse: newPaginatedResponseNoTotal(page, offset, hasMore),
		Mode:              mode,
		PoolSaturated:     meta.PoolSaturated,
		Generation: hybridGenerationSummary{
			ID:          int64(meta.Generation.ID),
			Model:       meta.Generation.Model,
			Dimension:   meta.Generation.Dimension,
			Fingerprint: meta.Generation.Fingerprint,
			State:       string(meta.Generation.State),
		},
	})
}

// similarMessagesResponse is the full response body for
// find_similar_messages.
type similarMessagesResponse struct {
	SeedMessageID int64                   `json:"seed_message_id"`
	Returned      int                     `json:"returned"`
	Generation    hybridGenerationSummary `json:"generation"`
	Messages      []query.MessageSummary  `json:"messages"`
}

// findSimilarMessages returns nearest-neighbour messages to a seed
// message using the active vector index. The seed is excluded from
// results. Structured filters (account, after, before, has_attachment)
// are applied at the backend level.
func (h *handlers) findSimilarMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.backend == nil {
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured on this server",
		), nil
	}
	args := req.GetArguments()

	seedID, err := getIDArg(args, "message_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := limitArg(args, "limit", 20)
	if maxPage := h.vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && limit > maxPage {
		limit = maxPage
	}

	filter, err := h.filterFromFindSimilarArgs(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	seed, err := h.backend.LoadVector(ctx, seedID)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("load seed vector: %v", err)), nil
	}

	active, err := h.backend.ActiveGeneration(ctx)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("active generation: %v", err)), nil
	}

	// +1 so we can drop the seed itself from results without coming up short.
	hits, err := h.backend.Search(ctx, active.ID, seed, limit+1, filter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	// Bulk-hydrate keeping rank order. Drop the seed first so the +1
	// over-fetch is paid for in the size budget rather than the
	// hydration round-trip.
	wantIDs := make([]int64, 0, limit)
	for _, hit := range hits {
		if hit.MessageID == seedID {
			continue
		}
		if len(wantIDs) >= limit {
			break
		}
		wantIDs = append(wantIDs, hit.MessageID)
	}
	summaries, err := h.engine.GetMessageSummariesByIDs(ctx, wantIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"mcp: hydrate similar hits failed: ids=%d error=%v\n",
			len(wantIDs), err)
		summaries = nil
	}
	byID := make(map[int64]query.MessageSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
	}
	messages := make([]query.MessageSummary, 0, len(wantIDs))
	for _, id := range wantIDs {
		if msg, ok := byID[id]; ok {
			messages = append(messages, msg)
		}
	}

	return jsonResult(similarMessagesResponse{
		SeedMessageID: seedID,
		Returned:      len(messages),
		Generation: hybridGenerationSummary{
			ID:          int64(active.ID),
			Model:       active.Model,
			Dimension:   active.Dimension,
			Fingerprint: active.Fingerprint,
			State:       string(active.State),
		},
		Messages: messages,
	})
}

// filterFromFindSimilarArgs builds a vector.Filter from the
// find_similar_messages args. Returns an error if account lookup fails.
// Sender/label filters are intentionally not exposed — resolving
// participant/label names to IDs requires a main-DB handle that the
// MCP handlers struct does not currently hold. A future task that
// wires the DB through can extend both the schema and this helper.
func (h *handlers) filterFromFindSimilarArgs(ctx context.Context, args map[string]any) (vector.Filter, error) {
	var f vector.Filter

	account, _ := args["account"].(string)
	srcID, err := h.getAccountID(ctx, account)
	if err != nil {
		return f, err
	}
	if srcID != nil {
		f.SourceIDs = []int64{*srcID}
	}

	if v, ok := args["has_attachment"].(bool); ok && v {
		tr := true
		f.HasAttachment = &tr
	}
	after, err := getDateArg(args, "after")
	if err != nil {
		return f, err
	}
	if after != nil {
		f.After = after
	}
	before, err := getDateArg(args, "before")
	if err != nil {
		return f, err
	}
	if before != nil {
		f.Before = before
	}
	return f, nil
}

func (h *handlers) getMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	msg, err := h.engine.GetMessage(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("message not found: %v", err)), nil
	}
	return jsonResult(msg)
}

const maxAttachmentSize = 50 * 1024 * 1024 // 50MB

func (h *handlers) getAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "attachment_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	att, err := h.engine.GetAttachment(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get attachment failed: %v", err)), nil
	}
	if att == nil {
		return mcp.NewToolResultError("attachment not found"), nil
	}

	if h.attachmentsDir == "" {
		return mcp.NewToolResultError("attachments directory not configured"), nil
	}

	if att.Size > maxAttachmentSize {
		return mcp.NewToolResultError(fmt.Sprintf("attachment too large: %d bytes (max %d)", att.Size, maxAttachmentSize)), nil
	}

	data, err := h.readAttachmentFile(att.ContentHash)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	mimeType := att.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	metaObj := struct {
		Filename string `json:"filename"`
		MimeType string `json:"mime_type"`
		Size     int64  `json:"size"`
	}{
		Filename: att.Filename,
		MimeType: mimeType,
		Size:     att.Size,
	}
	metaJSON, err := json.Marshal(metaObj)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal metadata: %v", err)), nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{
				Type: "text",
				Text: string(metaJSON),
			},
			mcp.EmbeddedResource{
				Type: "resource",
				Resource: mcp.BlobResourceContents{
					URI:      fmt.Sprintf("attachment:///%d/%s", att.ID, url.PathEscape(att.Filename)),
					MIMEType: mimeType,
					Blob:     base64.StdEncoding.EncodeToString(data),
				},
			},
		},
	}, nil
}

func (h *handlers) exportAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "attachment_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	att, err := h.engine.GetAttachment(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get attachment failed: %v", err)), nil
	}
	if att == nil {
		return mcp.NewToolResultError("attachment not found"), nil
	}

	if h.attachmentsDir == "" {
		return mcp.NewToolResultError("attachments directory not configured"), nil
	}

	if att.Size > maxAttachmentSize {
		return mcp.NewToolResultError(fmt.Sprintf("attachment too large: %d bytes (max %d)", att.Size, maxAttachmentSize)), nil
	}

	data, err := h.readAttachmentFile(att.ContentHash)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Determine destination directory.
	destDir, _ := args["destination"].(string)
	if destDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("cannot determine home directory: %v", err)), nil
		}
		destDir = filepath.Join(home, "Downloads")
	}

	info, err := os.Stat(destDir)
	if err != nil || !info.IsDir() {
		return mcp.NewToolResultError("destination directory does not exist: " + destDir), nil //nolint:nilerr // MCP convention: tool errors flow via ToolResultError, not Go error
	}

	// Sanitize and deduplicate filename.
	filename := export.SanitizeFilename(filepath.Base(att.Filename))
	if filename == "" || filename == "." {
		filename = att.ContentHash
	}
	f, outPath, err := export.CreateExclusiveFile(filepath.Join(destDir, filename), 0600)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", err)), nil
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(outPath)
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", writeErr)), nil
	}
	if closeErr != nil {
		_ = os.Remove(outPath)
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", closeErr)), nil
	}

	resp := struct {
		Path     string `json:"path"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}{
		Path:     outPath,
		Filename: filepath.Base(outPath),
		Size:     int64(len(data)),
	}
	return jsonResult(resp)
}

func (h *handlers) listMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	filter := query.MessageFilter{
		SourceID: sourceID,
		Pagination: query.Pagination{
			Limit:  listLimitArg(args) + 1,
			Offset: limitArg(args, "offset", 0),
		},
	}

	if v, ok := args["from"].(string); ok && v != "" {
		// If it looks like an email address, filter by email; otherwise by display name.
		if strings.Contains(v, "@") || strings.HasPrefix(v, "+") {
			filter.Sender = v
		} else {
			filter.SenderName = v
		}
	}
	if v, ok := args["to"].(string); ok && v != "" {
		filter.Recipient = v
	}
	if v, ok := args["label"].(string); ok && v != "" {
		filter.Label = v
	}
	if v, ok := args["has_attachment"].(bool); ok && v {
		filter.WithAttachmentsOnly = true
	}
	if filter.After, err = getDateArg(args, "after"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if filter.Before, err = getDateArg(args, "before"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	results, err := h.engine.ListMessages(ctx, filter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list failed: %v", err)), nil
	}

	pageLimit := listLimitArg(args)
	offset := filter.Pagination.Offset
	hasMore := len(results) > pageLimit
	if hasMore {
		results = results[:pageLimit]
	}

	if full, _ := args["full"].(bool); full {
		return jsonResult(newPaginatedResponseNoTotal(results, offset, hasMore))
	}

	// Compact summaries are the default for MCP to keep common mailbox lookups
	// cheap in both tokens and latency.
	return jsonResult(newPaginatedResponseNoTotal(compactMessageSummaries(results), offset, hasMore))
}

// getStatsResponse is the JSON body returned by the get_stats MCP tool.
// VectorSearch is omitempty so archives without vector search do not
// surface an empty sub-object to callers.
type getStatsResponse struct {
	Stats        *query.TotalStats   `json:"stats"`
	Accounts     []query.AccountInfo `json:"accounts"`
	VectorSearch *vector.StatsView   `json:"vector_search,omitempty"`
}

func (h *handlers) getStats(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	stats, err := h.engine.GetTotalStats(ctx, query.StatsOptions{})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stats failed: %v", err)), nil
	}

	accounts, err := h.engine.ListAccounts(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("accounts failed: %v", err)), nil
	}

	// Vector stats are best-effort: partial failures are logged here but
	// still attached to the response so callers see whatever succeeded.
	vs, vsErr := vector.CollectStats(ctx, h.backend)
	if vsErr != nil {
		fmt.Fprintf(os.Stderr, "mcp: vector stats failed: %v\n", vsErr)
	}

	return jsonResult(getStatsResponse{
		Stats:        stats,
		Accounts:     accounts,
		VectorSearch: vs,
	})
}

func (h *handlers) aggregate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	groupBy, _ := args["group_by"].(string)
	if groupBy == "" {
		return mcp.NewToolResultError("group_by parameter is required"), nil
	}

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	opts := query.AggregateOptions{
		SourceID: sourceID,
		Limit:    limitArg(args, "limit", 50),
	}

	if opts.After, err = getDateArg(args, "after"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if opts.Before, err = getDateArg(args, "before"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	viewTypeMap := map[string]query.ViewType{
		"sender":    query.ViewSenders,
		"recipient": query.ViewRecipients,
		"domain":    query.ViewDomains,
		"label":     query.ViewLabels,
		"time":      query.ViewTime,
	}

	viewType, ok := viewTypeMap[groupBy]
	if !ok {
		return mcp.NewToolResultError("invalid group_by: " + groupBy), nil
	}

	rows, err := h.engine.Aggregate(ctx, viewType, opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("aggregate failed: %v", err)), nil
	}

	return jsonResult(rows)
}

// limitArg extracts a non-negative integer limit from a map, with a default.
// JSON numbers arrive as float64. Clamps to maxLimit to prevent excessive
// result sets.
func limitArg(args map[string]any, key string, def int) int {
	v, ok := args[key].(float64)
	if !ok {
		return def
	}
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if math.IsInf(v, 1) || v > float64(maxLimit) {
		return maxLimit
	}
	return int(v)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// maxStageDeletionResults limits how many messages can be staged in one call.
const maxStageDeletionResults = 100000

func (h *handlers) stageDeletion(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Check for query vs structured filters
	queryStr, _ := args["query"].(string)
	queryStr = strings.TrimSpace(queryStr)
	hasQuery := queryStr != ""

	// Check for any structured filter
	fromStr, _ := args["from"].(string)
	domainStr, _ := args["domain"].(string)
	labelStr, _ := args["label"].(string)
	hasAttachment, _ := args["has_attachment"].(bool)
	afterDate, err := getDateArg(args, "after")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	beforeDate, err := getDateArg(args, "before")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	hasStructuredFilter := fromStr != "" || domainStr != "" || labelStr != "" ||
		hasAttachment || afterDate != nil || beforeDate != nil

	// Validate: must have either query or structured filters, but not both
	if hasQuery && hasStructuredFilter {
		return mcp.NewToolResultError("use either 'query' or structured filters (from, domain, label, etc.), not both"), nil
	}
	if !hasQuery && !hasStructuredFilter {
		return mcp.NewToolResultError("must provide either 'query' or at least one filter (from, domain, label, after, before, has_attachment)"), nil
	}

	var gmailIDs []string
	var description string

	if hasQuery {
		// Query-based search
		q := search.Parse(queryStr)
		if sourceID != nil {
			q.AccountIDs = []int64{*sourceID}
		}

		// Try fast search first
		filter := query.MessageFilter{SourceID: sourceID}
		results, err := h.engine.SearchFast(ctx, q, filter, maxStageDeletionResults, 0)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}

		// Fall back to FTS if no results and query has text terms
		if len(results) == 0 && len(q.TextTerms) > 0 {
			results, err = h.engine.Search(ctx, q, maxStageDeletionResults, 0)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
			}
		}

		for _, msg := range results {
			gmailIDs = append(gmailIDs, msg.SourceMessageID)
		}
		description = "query: " + queryStr
		if len(description) > 50 {
			description = description[:50]
		}
	} else {
		// Structured filter
		filter := query.MessageFilter{
			SourceID:            sourceID,
			Sender:              fromStr,
			Domain:              domainStr,
			Label:               labelStr,
			WithAttachmentsOnly: hasAttachment,
			After:               afterDate,
			Before:              beforeDate,
			Pagination: query.Pagination{
				Limit: maxStageDeletionResults,
			},
		}

		var err error
		gmailIDs, err = h.engine.GetGmailIDsByFilter(ctx, filter)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("filter failed: %v", err)), nil
		}

		// Build description from filters
		var parts []string
		if fromStr != "" {
			parts = append(parts, "from:"+fromStr)
		}
		if domainStr != "" {
			parts = append(parts, "domain:"+domainStr)
		}
		if labelStr != "" {
			parts = append(parts, "label:"+labelStr)
		}
		if hasAttachment {
			parts = append(parts, "has:attachment")
		}
		if afterDate != nil {
			parts = append(parts, "after:"+afterDate.Format("2006-01-02"))
		}
		if beforeDate != nil {
			parts = append(parts, "before:"+beforeDate.Format("2006-01-02"))
		}
		description = "filter: " + strings.Join(parts, " ")
		if len(description) > 50 {
			description = description[:50]
		}
	}

	if len(gmailIDs) == 0 {
		return mcp.NewToolResultError("no messages match the specified criteria"), nil
	}

	// Create deletion manager and manifest
	deletionsDir := filepath.Join(h.dataDir, "deletions")
	manager, err := deletion.NewManager(deletionsDir)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create deletion manager: %v", err)), nil
	}

	manifest := deletion.NewManifest(description, gmailIDs)
	manifest.CreatedBy = "mcp"

	// Set filter metadata for execution
	manifest.Filters.Account = account
	if fromStr != "" {
		manifest.Filters.Senders = []string{fromStr}
	}
	if domainStr != "" {
		manifest.Filters.SenderDomains = []string{domainStr}
	}
	if labelStr != "" {
		manifest.Filters.Labels = []string{labelStr}
	}
	if afterDate != nil {
		manifest.Filters.After = afterDate.Format("2006-01-02")
	}
	if beforeDate != nil {
		manifest.Filters.Before = beforeDate.Format("2006-01-02")
	}

	if err := manager.SaveManifest(manifest); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("save manifest: %v", err)), nil
	}

	resp := struct {
		BatchID      string `json:"batch_id"`
		MessageCount int    `json:"message_count"`
		Status       string `json:"status"`
		NextStep     string `json:"next_step"`
	}{
		BatchID:      manifest.ID,
		MessageCount: len(gmailIDs),
		Status:       string(manifest.Status),
		NextStep:     "Run 'MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged' to execute deletion (gated for v1), or 'msgvault cancel-deletion " + manifest.ID + "' to cancel",
	}

	return jsonResult(resp)
}

func (h *handlers) searchByDomains(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	domainsStr, _ := args["domains"].(string)
	domainsStr = strings.TrimSpace(domainsStr)
	if domainsStr == "" {
		return mcp.NewToolResultError("domains is required"), nil
	}

	// Split and clean domain list
	var domains []string
	for d := range strings.SplitSeq(domainsStr, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			domains = append(domains, d)
		}
	}
	if len(domains) == 0 {
		return mcp.NewToolResultError("at least one domain is required"), nil
	}

	limit := limitArg(args, "limit", 100)
	offset := limitArg(args, "offset", 0)

	afterDate, err := getDateArg(args, "after")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	beforeDate, err := getDateArg(args, "before")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	results, err := h.engine.SearchByDomains(ctx, domains, afterDate, beforeDate, limit, offset)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search by domains failed: %v", err)), nil
	}

	return jsonResult(results)
}

// --- WhatsApp handlers ---

func (h *handlers) getWhatsAppClient(ctx context.Context, args map[string]any) (whatsapplive.Client, string, error) {
	if h.whatsAppFactory == nil {
		return nil, "", fmt.Errorf("WhatsApp live client not configured")
	}

	account, _ := args["account"].(string)
	account = strings.TrimSpace(account)

	accounts, err := h.engine.ListAccounts(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("failed to list accounts: %w", err)
	}
	whatsAppAccounts := make([]query.AccountInfo, 0, len(accounts))
	for _, acc := range accounts {
		if acc.SourceType == store.WhatsAppSourceType {
			whatsAppAccounts = append(whatsAppAccounts, acc)
		}
	}

	if account == "" {
		switch len(whatsAppAccounts) {
		case 0:
			// Allow the live client to report pairing status before the first
			// archive source has been created.
		case 1:
			account = whatsAppAccounts[0].Identifier
		default:
			return nil, "", fmt.Errorf("multiple WhatsApp accounts configured, specify 'account' parameter")
		}
	} else if len(whatsAppAccounts) > 0 {
		found := false
		for _, acc := range whatsAppAccounts {
			if acc.Identifier == account {
				found = true
				break
			}
		}
		if !found {
			return nil, "", fmt.Errorf("WhatsApp account not found: %s", account)
		}
	}

	client, err := h.whatsAppFactory(ctx, account)
	if err != nil {
		return nil, "", fmt.Errorf("open WhatsApp client: %w", err)
	}
	return client, account, nil
}

func (h *handlers) whatsAppStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	client, _, err := h.getWhatsAppClient(ctx, req.GetArguments())
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	status, err := client.Status(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("whatsapp status: %v", err)), nil
	}
	return jsonResult(status)
}

func (h *handlers) sendWhatsAppMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	client, account, err := h.getWhatsAppClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	chatID, _ := args["chat_id"].(string)
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return mcp.NewToolResultError("chat_id parameter is required"), nil
	}
	body, _ := args["body"].(string)
	body = strings.TrimSpace(body)
	if body == "" {
		return mcp.NewToolResultError("body parameter is required"), nil
	}
	localRequestID, _ := args["local_request_id"].(string)

	result, err := client.SendMessage(ctx, whatsapplive.SendMessageRequest{
		Account:        account,
		ChatID:         chatID,
		Body:           body,
		LocalRequestID: strings.TrimSpace(localRequestID),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("send whatsapp message: %v", err)), nil
	}
	return jsonResult(result)
}

func (h *handlers) sendWhatsAppReaction(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	client, account, err := h.getWhatsAppClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	messageID, err := getIDArg(args, "message_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	emojiRaw, ok := args["emoji"]
	if !ok {
		return mcp.NewToolResultError("emoji parameter is required; use an empty string to clear"), nil
	}
	emoji, ok := emojiRaw.(string)
	if !ok {
		return mcp.NewToolResultError("emoji must be a string"), nil
	}
	localRequestID, _ := args["local_request_id"].(string)

	result, err := client.SendReaction(ctx, whatsapplive.SendReactionRequest{
		Account:        account,
		MessageID:      messageID,
		Emoji:          emoji,
		LocalRequestID: strings.TrimSpace(localRequestID),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("send whatsapp reaction: %v", err)), nil
	}
	return jsonResult(result)
}

// --- Google Docs handlers ---

type googleDocsSearchResult struct {
	googledocs.File
	Snippet       string `json:"snippet"`
	TextLength    int    `json:"text_length"`
	TextTruncated bool   `json:"text_truncated"`
}

func (h *handlers) getGoogleDocsClient(ctx context.Context) (googledocs.Client, error) {
	if h.googleDocsFactory == nil {
		return nil, fmt.Errorf("Google Docs API not configured")
	}
	client, err := h.googleDocsFactory(ctx)
	if err != nil {
		return nil, fmt.Errorf("open Google Docs client: %w", err)
	}
	return client, nil
}

func (h *handlers) listGoogleDocs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	client, err := h.getGoogleDocsClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	files, err := client.ListDocs(
		ctx,
		optionalStringArg(args, "source"),
		optionalStringArg(args, "query"),
		boundedIntArg(args, "limit", defaultGoogleDocsListLimit, maxGoogleDocsListLimit),
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list Google Docs: %v", err)), nil
	}
	return jsonResult(files)
}

func (h *handlers) searchGoogleDocs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	query, err := requiredTrimmedStringArg(args, "query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	client, err := h.getGoogleDocsClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	limit := boundedIntArg(args, "limit", defaultGoogleDocsSearchLimit, maxGoogleDocsSearchLimit)
	snippetChars := boundedIntArg(args, "snippet_chars", defaultGoogleDocsSnippetChars, maxGoogleDocsSnippetChars)
	files, err := client.ListDocs(ctx, optionalStringArg(args, "source"), query, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search Google Docs: %v", err)), nil
	}
	results := make([]googleDocsSearchResult, 0, len(files))
	for _, file := range files {
		doc, err := client.GetDoc(ctx, file.Source, file.DocumentID, maxGoogleDocsMaxChars)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("get Google Doc %s: %v", file.DocumentID, err)), nil
		}
		results = append(results, googleDocsSearchResult{
			File:          file,
			Snippet:       googleDocsSnippet(doc.Text, query, snippetChars),
			TextLength:    doc.TextLength,
			TextTruncated: doc.TextTruncated,
		})
	}
	return jsonResult(results)
}

func (h *handlers) getGoogleDoc(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	documentID, err := requiredTrimmedStringArg(args, "document_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	client, err := h.getGoogleDocsClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	doc, err := client.GetDoc(
		ctx,
		optionalStringArg(args, "source"),
		documentID,
		boundedIntArg(args, "max_chars", defaultGoogleDocsMaxChars, maxGoogleDocsMaxChars),
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get Google Doc: %v", err)), nil
	}
	return jsonResult(doc)
}

func (h *handlers) appendGoogleDocText(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	documentID, err := requiredTrimmedStringArg(args, "document_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	text, err := requiredStringArg(args, "text")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	client, err := h.getGoogleDocsClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result, err := client.AppendText(ctx, optionalStringArg(args, "source"), documentID, text)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("append Google Doc text: %v", err)), nil
	}
	return jsonResult(result)
}

func (h *handlers) replaceGoogleDocText(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	documentID, err := requiredTrimmedStringArg(args, "document_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	find, err := requiredStringArg(args, "find")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	replacement, err := requiredStringArgAllowEmpty(args, "replacement")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	matchCase, _ := args["match_case"].(bool)
	client, err := h.getGoogleDocsClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result, err := client.ReplaceText(ctx, optionalStringArg(args, "source"), documentID, find, replacement, matchCase)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("replace Google Doc text: %v", err)), nil
	}
	return jsonResult(result)
}

func optionalStringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func requiredTrimmedStringArg(args map[string]any, key string) (string, error) {
	v, err := requiredStringArg(args, key)
	if err != nil {
		return "", err
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("%s parameter is required", key)
	}
	return v, nil
}

func requiredStringArg(args map[string]any, key string) (string, error) {
	v, ok := args[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("%s parameter is required", key)
	}
	return v, nil
}

func requiredStringArgAllowEmpty(args map[string]any, key string) (string, error) {
	v, ok := args[key].(string)
	if !ok {
		return "", fmt.Errorf("%s parameter is required", key)
	}
	return v, nil
}

func boundedIntArg(args map[string]any, key string, def, maxValue int) int {
	v := limitArg(args, key, def)
	if v <= 0 {
		return def
	}
	return min(v, maxValue)
}

func googleDocsSnippet(text, query string, maxChars int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	maxChars = min(max(maxChars, 1), maxGoogleDocsSnippetChars)
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	startRune := 0
	if needle != "" {
		if idx := strings.Index(strings.ToLower(text), needle); idx >= 0 {
			startRune = utf8.RuneCountInString(text[:idx]) - maxChars/4
			if startRune < 0 {
				startRune = 0
			}
		}
	}
	endRune := min(len(runes), startRune+maxChars)
	snippet := string(runes[startRune:endRune])
	if startRune > 0 {
		snippet = "..." + snippet
	}
	if endRune < len(runes) {
		snippet += "..."
	}
	return snippet
}

// --- Draft handlers ---

// getGmailClient resolves the account email and returns an authenticated Gmail client.
// The caller must close the returned client.
func (h *handlers) getGmailClient(ctx context.Context, args map[string]any) (*gmail.Client, string, error) {
	if h.gmailFactory == nil {
		return nil, "", fmt.Errorf("Gmail API not configured (OAuth credentials needed)")
	}

	account, _ := args["account"].(string)
	if account == "" {
		// If no account specified, try to use the first/only account
		accounts, err := h.engine.ListAccounts(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("failed to list accounts: %w", err)
		}
		if len(accounts) == 0 {
			return nil, "", fmt.Errorf("no accounts configured")
		}
		if len(accounts) > 1 {
			return nil, "", fmt.Errorf("multiple accounts configured, specify 'account' parameter (use get_stats to list accounts)")
		}
		account = accounts[0].Identifier
	}

	client, err := h.gmailFactory(ctx, account)
	if err != nil {
		return nil, "", fmt.Errorf("authenticate %s: %w", account, err)
	}

	return client, account, nil
}

func (h *handlers) listDrafts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	queryStr, _ := args["query"].(string)
	limit := limitArg(args, "limit", 20)

	drafts, err := client.ListDrafts(ctx, queryStr, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list drafts: %v", err)), nil
	}

	// Return a compact summary
	type draftSummary struct {
		DraftID string   `json:"draft_id"`
		Subject string   `json:"subject"`
		To      []string `json:"to,omitempty"`
		Snippet string   `json:"snippet"`
		BodyLen int      `json:"body_length"`
	}

	summaries := make([]draftSummary, len(drafts))
	for i, d := range drafts {
		summaries[i] = draftSummary{
			DraftID: d.ID,
			Subject: d.Message.Subject,
			To:      d.Message.To,
			Snippet: d.Message.Snippet,
			BodyLen: len(d.Message.Body),
		}
	}

	return jsonResult(summaries)
}

func (h *handlers) getDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	draftID, _ := args["draft_id"].(string)
	if draftID == "" {
		return mcp.NewToolResultError("draft_id parameter is required"), nil
	}

	draft, err := client.GetDraft(ctx, draftID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get draft: %v", err)), nil
	}

	return jsonResult(draft)
}

func (h *handlers) createDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	body, _ := args["body"].(string)
	if body == "" {
		return mcp.NewToolResultError("body parameter is required"), nil
	}

	compose := &gmail.DraftCompose{
		Body: body,
	}

	if v, _ := args["to"].(string); v != "" {
		compose.To = splitCSV(v)
	}
	if v, _ := args["cc"].(string); v != "" {
		compose.Cc = splitCSV(v)
	}
	if v, _ := args["bcc"].(string); v != "" {
		compose.Bcc = splitCSV(v)
	}
	if v, _ := args["subject"].(string); v != "" {
		compose.Subject = v
	}
	if v, _ := args["content_type"].(string); v != "" {
		compose.ContentType = v
	}
	if v, _ := args["thread_id"].(string); v != "" {
		compose.ThreadID = v
	}
	if v, _ := args["in_reply_to"].(string); v != "" {
		compose.InReplyTo = v
	}
	if v, _ := args["references"].(string); v != "" {
		compose.References = v
	}

	atts, err := h.resolveDraftAttachments(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	compose.Attachments = atts

	draft, err := client.CreateDraft(ctx, compose)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create draft: %v", err)), nil
	}

	resp := struct {
		DraftID     string `json:"draft_id"`
		Subject     string `json:"subject"`
		Attachments int    `json:"attachments"`
		NextStep    string `json:"next_step"`
	}{
		DraftID:     draft.ID,
		Subject:     draft.Message.Subject,
		Attachments: len(atts),
		NextStep:    "Use send_draft to send, update_draft to modify, or delete_draft to discard",
	}

	return jsonResult(resp)
}

func (h *handlers) updateDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	draftID, _ := args["draft_id"].(string)
	if draftID == "" {
		return mcp.NewToolResultError("draft_id parameter is required"), nil
	}

	body, _ := args["body"].(string)
	if body == "" {
		return mcp.NewToolResultError("body parameter is required"), nil
	}

	compose := &gmail.DraftCompose{
		Body: body,
	}

	if v, _ := args["to"].(string); v != "" {
		compose.To = splitCSV(v)
	}
	if v, _ := args["cc"].(string); v != "" {
		compose.Cc = splitCSV(v)
	}
	if v, _ := args["bcc"].(string); v != "" {
		compose.Bcc = splitCSV(v)
	}
	if v, _ := args["subject"].(string); v != "" {
		compose.Subject = v
	}
	if v, _ := args["content_type"].(string); v != "" {
		compose.ContentType = v
	}
	if v, _ := args["thread_id"].(string); v != "" {
		compose.ThreadID = v
	}
	if v, _ := args["in_reply_to"].(string); v != "" {
		compose.InReplyTo = v
	}
	if v, _ := args["references"].(string); v != "" {
		compose.References = v
	}

	atts, err := h.resolveDraftAttachments(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	compose.Attachments = atts

	draft, err := client.UpdateDraft(ctx, draftID, compose)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update draft: %v", err)), nil
	}

	return jsonResult(struct {
		DraftID string `json:"draft_id"`
		Subject string `json:"subject"`
		Status  string `json:"status"`
	}{
		DraftID: draft.ID,
		Subject: draft.Message.Subject,
		Status:  "updated",
	})
}

func (h *handlers) deleteDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	draftID, _ := args["draft_id"].(string)
	if draftID == "" {
		return mcp.NewToolResultError("draft_id parameter is required"), nil
	}

	if err := client.DeleteDraft(ctx, draftID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete draft: %v", err)), nil
	}

	return jsonResult(struct {
		DraftID string `json:"draft_id"`
		Status  string `json:"status"`
	}{
		DraftID: draftID,
		Status:  "deleted",
	})
}

func (h *handlers) sendDraft(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	draftID, _ := args["draft_id"].(string)
	if draftID == "" {
		return mcp.NewToolResultError("draft_id parameter is required"), nil
	}

	sent, err := client.SendDraft(ctx, draftID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("send draft: %v", err)), nil
	}

	return jsonResult(struct {
		MessageID string   `json:"message_id"`
		ThreadID  string   `json:"thread_id"`
		LabelIDs  []string `json:"label_ids"`
		Status    string   `json:"status"`
	}{
		MessageID: sent.ID,
		ThreadID:  sent.ThreadID,
		LabelIDs:  sent.LabelIDs,
		Status:    "sent",
	})
}

// --- Label handlers ---

func (h *handlers) modifyLabels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	messageIDsStr, _ := args["message_ids"].(string)
	if messageIDsStr == "" {
		return mcp.NewToolResultError("message_ids parameter is required"), nil
	}

	addLabelsStr, _ := args["add_labels"].(string)
	removeLabelsStr, _ := args["remove_labels"].(string)

	if addLabelsStr == "" && removeLabelsStr == "" {
		return mcp.NewToolResultError("at least one of add_labels or remove_labels is required"), nil
	}

	messageIDs := splitCSV(messageIDsStr)
	addLabelIDs := splitCSV(addLabelsStr)
	removeLabelIDs := splitCSV(removeLabelsStr)

	if len(messageIDs) > 1 {
		err = client.BatchModifyLabels(ctx, messageIDs, addLabelIDs, removeLabelIDs)
	} else {
		err = client.ModifyMessageLabels(ctx, messageIDs[0], addLabelIDs, removeLabelIDs)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("modify labels: %v", err)), nil
	}

	return jsonResult(struct {
		MessageCount int      `json:"message_count"`
		Added        []string `json:"added_labels,omitempty"`
		Removed      []string `json:"removed_labels,omitempty"`
		Status       string   `json:"status"`
	}{
		MessageCount: len(messageIDs),
		Added:        addLabelIDs,
		Removed:      removeLabelIDs,
		Status:       "modified",
	})
}

func (h *handlers) createLabel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	name, _ := args["name"].(string)
	if name == "" {
		return mcp.NewToolResultError("name parameter is required"), nil
	}

	label, err := client.CreateLabel(ctx, name)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create label: %v", err)), nil
	}

	return jsonResult(label)
}

func (h *handlers) deleteLabel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	labelID, _ := args["label_id"].(string)
	if labelID == "" {
		return mcp.NewToolResultError("label_id parameter is required"), nil
	}

	if err := client.DeleteLabel(ctx, labelID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete label: %v", err)), nil
	}

	return jsonResult(struct {
		LabelID string `json:"label_id"`
		Status  string `json:"status"`
	}{
		LabelID: labelID,
		Status:  "deleted",
	})
}

func (h *handlers) listGmailLabels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	client, _, err := h.getGmailClient(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer client.Close()

	labels, err := client.ListLabels(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list labels: %v", err)), nil
	}

	return jsonResult(labels)
}

// splitCSV splits a comma-separated string into trimmed parts.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

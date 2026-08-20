package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

type ocrSearchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type ocrSearchResponse struct {
	Results []store.OCRSearchHit `json:"results"`
}

func (s *Server) registerOCRRoutes(apiV1 huma.API) {
	registerAPIV1RawHumaJSONRoute[store.OCRRuntimeStatus](apiV1, "getOCRStatus", http.MethodGet, "/ocr/status", "Get attachment text extraction status", s.handleOCRStatus)
	registerAPIV1RawHumaJSONRoute[store.OCRResult](apiV1, "getAttachmentText", http.MethodGet, "/attachments/{hash}/text", "Get cached attachment text", s.handleGetOCR)
	registerAPIV1RawHumaJSONRoute[store.OCRResult](apiV1, "requestAttachmentText", http.MethodPost, "/attachments/{hash}/text/request", "Queue attachment text extraction", s.handleRequestOCR)
	registerAPIV1RawHumaJSONRoute[ocrSearchResponse](apiV1, "searchAttachmentText", http.MethodPost, "/files/text-search", "Search cached attachment text", s.handleSearchOCR)
}

func (s *Server) handleOCRStatus(w http.ResponseWriter, r *http.Request) {
	if s.ocrStore == nil {
		writeError(w, http.StatusServiceUnavailable, "ocr_unavailable", "OCR persistence is unavailable")
		return
	}
	summary, err := s.ocrStore.OCRSummary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ocr_status_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, store.OCRRuntimeStatus{Enabled: s.cfg.OCR.Enabled, Fingerprint: s.cfg.OCR.Fingerprint, OCRSummary: summary})
}

func (s *Server) handleGetOCR(w http.ResponseWriter, r *http.Request) {
	if s.ocrStore == nil {
		writeError(w, http.StatusServiceUnavailable, "ocr_unavailable", "OCR persistence is unavailable")
		return
	}
	result, err := s.ocrStore.GetOCRResult(r.Context(), ocrHash(r), true)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "ocr_not_found", "No OCR result exists for this attachment")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ocr_read_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleRequestOCR(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.OCR.Enabled {
		writeError(w, http.StatusServiceUnavailable, "ocr_disabled", "Attachment OCR is disabled")
		return
	}
	if s.ocrStore == nil {
		writeError(w, http.StatusServiceUnavailable, "ocr_unavailable", "OCR persistence is unavailable")
		return
	}
	result, err := s.ocrStore.RequestOCR(r.Context(), ocrHash(r), s.cfg.OCR.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "attachment_not_found", "Attachment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ocr_request_failed", err.Error())
		return
	}
	if s.scheduler != nil && s.scheduler.IsJobScheduled("attachment-ocr") {
		_ = s.scheduler.TriggerJob("attachment-ocr")
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) handleSearchOCR(w http.ResponseWriter, r *http.Request) {
	if s.ocrStore == nil {
		writeError(w, http.StatusServiceUnavailable, "ocr_unavailable", "OCR persistence is unavailable")
		return
	}
	var req ocrSearchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query_required", "query is required")
		return
	}
	hits, err := s.ocrStore.SearchOCR(r.Context(), req.Query, req.Limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ocr_search_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ocrSearchResponse{Results: hits})
}

func ocrHash(r *http.Request) string {
	if value := r.PathValue("hash"); value != "" {
		return strings.ToLower(value)
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i := range parts {
		if parts[i] == "attachments" && i+1 < len(parts) {
			return strings.ToLower(parts[i+1])
		}
	}
	return ""
}

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	msgexport "go.kenn.io/msgvault/internal/export"
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
	registerAPIV1RawHumaJSONRoute[store.OCRResult](apiV1, "requestAttachmentText", http.MethodPost, "/attachments/{hash}/text/request", "Queue attachment text extraction", s.handleRequestOCR, http.StatusAccepted)
	registerAPIV1RawHumaJSONRouteWithRequest[ocrSearchRequest, ocrSearchResponse](apiV1, "searchAttachmentText", http.MethodPost, "/files/text-search", "Search cached attachment text", s.handleSearchOCR)
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
	hash, ok := validatedOCRHash(w, r)
	if !ok {
		return
	}
	result, err := s.ocrStore.GetOCRResult(r.Context(), hash, true)
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
	hash, ok := validatedOCRHash(w, r)
	if !ok {
		return
	}
	result, err := s.ocrStore.RequestOCR(r.Context(), hash, s.cfg.OCR.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "attachment_not_found", "Attachment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ocr_request_failed", err.Error())
		return
	}
	if s.scheduler != nil && s.scheduler.IsJobScheduled("attachment-ocr") {
		_ = s.scheduler.StartJob("attachment-ocr")
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
	if errors.Is(err, store.ErrOCRSearchUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "ocr_search_unavailable", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ocr_search_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ocrSearchResponse{Results: hits})
}

func validatedOCRHash(w http.ResponseWriter, r *http.Request) (string, bool) {
	hash := strings.ToLower(r.PathValue("hash"))
	if err := msgexport.ValidateContentHash(hash); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_hash", "Attachment hash must be a 64-character hex SHA-256")
		return "", false
	}
	return hash, true
}

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

type apiOCRStore struct {
	requested int
	searchErr error
}

func TestServerUsesExplicitOCRStore(t *testing.T) {
	ocrStore := &apiOCRStore{}
	server := NewServerWithOptions(ServerOptions{
		Config: &config.Config{}, Store: &mockStore{}, OCRStore: ocrStore,
	})
	assert.Same(t, ocrStore, server.ocrStore)
}

func (*apiOCRStore) OCRSummary(context.Context) (store.OCRSummary, error) {
	return store.OCRSummary{}, nil
}

func (s *apiOCRStore) RequestOCR(_ context.Context, hash, fingerprint string) (*store.OCRResult, error) {
	s.requested++
	return &store.OCRResult{ContentHash: hash, Fingerprint: fingerprint, Status: store.OCRPending}, nil
}

func (*apiOCRStore) GetOCRResult(context.Context, string, bool) (*store.OCRResult, error) {
	return nil, nil
}

func (s *apiOCRStore) SearchOCR(context.Context, string, int) ([]store.OCRSearchHit, error) {
	return nil, s.searchErr
}

func TestRequestOCRValidatesHashAndStartsJobAsynchronously(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ocrStore := &apiOCRStore{}
	scheduler := &mockScheduler{scheduledJobs: map[string]bool{"attachment-ocr": true}}
	server := &Server{
		cfg:      &config.Config{OCR: config.OCRConfig{Enabled: true, Fingerprint: "v1"}},
		ocrStore: ocrStore, scheduler: scheduler,
	}

	invalid := httptest.NewRequest(http.MethodPost, "/api/v1/attachments/bad/text/request", nil)
	invalid.SetPathValue("hash", "bad")
	invalidResponse := httptest.NewRecorder()
	server.handleRequestOCR(invalidResponse, invalid)
	assertions.Equal(http.StatusBadRequest, invalidResponse.Code)
	assertions.Zero(ocrStore.requested)

	hash := strings.Repeat("ab", 32)
	valid := httptest.NewRequest(http.MethodPost, "/api/v1/attachments/"+hash+"/text/request", nil)
	valid.SetPathValue("hash", hash)
	validResponse := httptest.NewRecorder()
	server.handleRequestOCR(validResponse, valid)
	requirements.Equal(http.StatusAccepted, validResponse.Code, validResponse.Body.String())
	assertions.Equal(1, ocrStore.requested)
	assertions.Equal([]string{"attachment-ocr"}, scheduler.startedJobs)
	assertions.Empty(scheduler.triggeredJobs)
}

func TestSearchOCRMapsUnavailableIndexToServiceUnavailable(t *testing.T) {
	server := &Server{
		cfg:      &config.Config{},
		ocrStore: &apiOCRStore{searchErr: store.ErrOCRSearchUnavailable},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/files/text-search", strings.NewReader(`{"query":"invoice"}`))
	response := httptest.NewRecorder()
	server.handleSearchOCR(response, request)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), "ocr_search_unavailable")
}

func TestOCROpenAPIPathsDeclareHashAndAcceptedResponse(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	doc := OpenAPIDocument()
	get := doc.Paths["/api/v1/attachments/{hash}/text"].Get
	request := doc.Paths["/api/v1/attachments/{hash}/text/request"].Post
	requirements.NotNil(get)
	requirements.NotNil(request)
	for _, operation := range []*huma.Operation{get, request} {
		requirements.Len(operation.Parameters, 1)
		assertions.Equal("hash", operation.Parameters[0].Name)
		assertions.Equal("path", operation.Parameters[0].In)
		assertions.True(operation.Parameters[0].Required)
	}
	assertions.Contains(request.Responses, "202")
}

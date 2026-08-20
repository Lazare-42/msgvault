package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"go.kenn.io/msgvault/internal/store"
)

func (c *Client) ocrJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := doRequestWithRootContext(c.requestContext(), c.httpClient, req)
	if err != nil {
		return fmt.Errorf("OCR daemon request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return HandleErrorResponse(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode OCR daemon response: %w", err)
	}
	return nil
}

func (c *Client) SearchOCR(ctx context.Context, query string, limit int) ([]store.OCRSearchHit, error) {
	var out struct {
		Results []store.OCRSearchHit `json:"results"`
	}
	err := c.ocrJSON(ctx, http.MethodPost, "/api/v1/files/text-search", map[string]any{"query": query, "limit": limit}, &out)
	return out.Results, err
}

func (c *Client) OCRStatus(ctx context.Context) (*store.OCRRuntimeStatus, error) {
	var out store.OCRRuntimeStatus
	err := c.ocrJSON(ctx, http.MethodGet, "/api/v1/ocr/status", nil, &out)
	return &out, err
}

func (c *Client) GetOCRResult(ctx context.Context, hash string, _ bool) (*store.OCRResult, error) {
	var out store.OCRResult
	err := c.ocrJSON(ctx, http.MethodGet, "/api/v1/attachments/"+url.PathEscape(hash)+"/text", nil, &out)
	return &out, err
}

func (c *Client) RequestOCR(ctx context.Context, hash, _ string) (*store.OCRResult, error) {
	var out store.OCRResult
	err := c.ocrJSON(ctx, http.MethodPost, "/api/v1/attachments/"+url.PathEscape(hash)+"/text/request", nil, &out)
	return &out, err
}

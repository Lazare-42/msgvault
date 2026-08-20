package ocr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Client struct {
	http *http.Client
}

func NewClient(socket string, timeout time.Duration) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: transport, Timeout: timeout}}
}

func (c *Client) Extract(ctx context.Context, filename, mimeType string, size int64, body io.Reader) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/extract", body)
	if err != nil {
		return Response{}, fmt.Errorf("create OCR request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("X-Filename", filename)
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("call OCR executor: %w", err)
	}
	defer resp.Body.Close()
	var out Response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<20)).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("decode OCR response: %w", err)
	}
	return out, nil
}

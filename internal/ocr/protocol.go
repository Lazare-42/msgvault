package ocr

import "go.kenn.io/msgvault/internal/store"

type Limits struct {
	MaxFileBytes   int64 `json:"max_file_bytes"`
	MaxPages       int   `json:"max_pages"`
	MaxPixels      int64 `json:"max_pixels"`
	MaxOutputBytes int64 `json:"max_output_bytes"`
}

type Response struct {
	Method    string          `json:"method,omitempty"`
	Pages     []store.OCRPage `json:"pages,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	Error     string          `json:"error,omitempty"`
	Permanent bool            `json:"permanent,omitempty"`
}

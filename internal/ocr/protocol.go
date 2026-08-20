package ocr

import (
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const (
	DefaultLanguages             = "fra+eng"
	DefaultDPI                   = 200
	DefaultRequestTimeout        = 10 * time.Minute
	DefaultMaxFileBytes          = int64(100 << 20)
	DefaultMaxPages              = 200
	DefaultMaxPixels             = int64(40_000_000)
	DefaultMaxPreprocessBytes    = int64(64 << 20)
	DefaultMaxOutputBytes        = int64(16 << 20)
	DefaultMaxAttempts           = 5
	DefaultMinImageSide          = 320
	DefaultMaxImageScale         = 4
	MaxConfiguredPixels          = int64(100_000_000)
	MaxConfiguredPreprocessBytes = int64(512 << 20)
)

func DefaultSocket(homeDir string) string {
	return filepath.Join(homeDir, "ocr", "executor.sock")
}

type Limits struct {
	MaxFileBytes       int64 `json:"max_file_bytes"`
	MaxPages           int   `json:"max_pages"`
	MaxPixels          int64 `json:"max_pixels"`
	MaxPreprocessBytes int64 `json:"max_preprocess_bytes"`
	MaxOutputBytes     int64 `json:"max_output_bytes"`
}

func ApplyLimitDefaults(limits *Limits) {
	if limits.MaxFileBytes <= 0 {
		limits.MaxFileBytes = DefaultMaxFileBytes
	}
	if limits.MaxPages <= 0 {
		limits.MaxPages = DefaultMaxPages
	}
	if limits.MaxPixels <= 0 {
		limits.MaxPixels = DefaultMaxPixels
	}
	if limits.MaxPreprocessBytes <= 0 {
		limits.MaxPreprocessBytes = DefaultMaxPreprocessBytes
	}
	if limits.MaxOutputBytes <= 0 {
		limits.MaxOutputBytes = DefaultMaxOutputBytes
	}
}

type Response struct {
	Method    string          `json:"method,omitempty"`
	Pages     []store.OCRPage `json:"pages,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	Error     string          `json:"error,omitempty"`
	Permanent bool            `json:"permanent,omitempty"`
}

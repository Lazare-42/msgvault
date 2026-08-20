package ocr

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type WorkStore interface {
	EnqueueOCRBacklog(context.Context, string, int) (int64, error)
	ClaimOCRJob(context.Context, string, time.Duration) (*store.OCRJob, error)
	CompleteOCR(context.Context, string, string, string, []store.OCRPage) error
	FailOCR(context.Context, string, string, string, string, bool, time.Duration) error
}

type BlobStore interface {
	OpenStream(context.Context, string) (io.ReadCloser, int64, error)
}

type Extractor interface {
	Extract(context.Context, string, string, int64, io.Reader) (Response, error)
}

type WorkerConfig struct {
	Fingerprint  string
	BatchSize    int
	Lease        time.Duration
	MaxFileBytes int64
	MaxAttempts  int
}

type RunResult struct {
	Discovered int64 `json:"discovered"`
	Processed  int   `json:"processed"`
	Succeeded  int   `json:"succeeded"`
	Failed     int   `json:"failed"`
}

type Worker struct {
	store     WorkStore
	blobs     BlobStore
	extractor Extractor
	cfg       WorkerConfig
	log       *slog.Logger
}

func NewWorker(workStore WorkStore, blobs BlobStore, extractor Extractor, cfg WorkerConfig, logger *slog.Logger) *Worker {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 15 * time.Minute
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 100 << 20
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: workStore, blobs: blobs, extractor: extractor, cfg: cfg, log: logger}
}

func (w *Worker) RunOnce(ctx context.Context) (RunResult, error) {
	var result RunResult
	discovered, err := w.store.EnqueueOCRBacklog(ctx, w.cfg.Fingerprint, max(100, w.cfg.BatchSize*10))
	if err != nil {
		return result, err
	}
	result.Discovered = discovered
	for result.Processed < w.cfg.BatchSize {
		job, err := w.store.ClaimOCRJob(ctx, w.cfg.Fingerprint, w.cfg.Lease)
		if err != nil {
			return result, err
		}
		if job == nil {
			break
		}
		result.Processed++
		if err := w.process(ctx, job); err != nil {
			result.Failed++
			w.log.Warn("OCR attachment failed", "content_hash", job.ContentHash, "error", err)
			continue
		}
		result.Succeeded++
	}
	return result, nil
}

func (w *Worker) process(ctx context.Context, job *store.OCRJob) error {
	if job.Size > w.cfg.MaxFileBytes {
		return w.recordFailure(ctx, job, "too_large", fmt.Sprintf("attachment is %d bytes; limit is %d", job.Size, w.cfg.MaxFileBytes), true)
	}
	reader, size, err := w.blobs.OpenStream(ctx, job.ContentHash)
	if err != nil {
		return w.recordFailure(ctx, job, "blob_unavailable", err.Error(), false)
	}
	resp, extractErr := w.extractor.Extract(ctx, job.Filename, job.MIMEType, size, reader)
	closeErr := reader.Close()
	if extractErr != nil || closeErr != nil {
		return w.recordFailure(ctx, job, "executor_unavailable", fmt.Sprint(errorsJoin(extractErr, closeErr)), false)
	}
	if resp.ErrorCode != "" {
		return w.recordFailure(ctx, job, resp.ErrorCode, resp.Error, resp.Permanent)
	}
	if err := w.store.CompleteOCR(ctx, job.ContentHash, w.cfg.Fingerprint, resp.Method, resp.Pages); err != nil {
		return fmt.Errorf("store OCR result: %w", err)
	}
	return nil
}

func (w *Worker) recordFailure(ctx context.Context, job *store.OCRJob, code, detail string, permanent bool) error {
	if job.Attempts >= w.cfg.MaxAttempts {
		permanent = true
		code = "attempts_exhausted"
	}
	backoff := time.Duration(math.Pow(2, float64(max(0, job.Attempts-1)))) * time.Minute
	if err := w.store.FailOCR(ctx, job.ContentHash, w.cfg.Fingerprint, code, detail, permanent, backoff); err != nil {
		return fmt.Errorf("record OCR failure after %s: %w", code, err)
	}
	return fmt.Errorf("%s: %s", code, detail)
}

func errorsJoin(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return fmt.Errorf("%v; %w", a, b)
}

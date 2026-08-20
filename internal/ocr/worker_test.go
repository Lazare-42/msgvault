package ocr

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type workerTestStore struct {
	completed bool
	failed    bool
	status    string
}

func (*workerTestStore) EnqueueOCRBacklog(context.Context, string, int) (int64, error) {
	return 0, nil
}

func (*workerTestStore) ClaimOCRJob(context.Context, string, time.Duration) (*store.OCRJob, error) {
	return nil, nil
}

func (s *workerTestStore) CompleteOCR(context.Context, string, string, int, string, []store.OCRPage) error {
	s.completed = true
	return nil
}

func (s *workerTestStore) FailOCR(_ context.Context, _, _ string, _ int, _, _, status string, _ time.Duration) error {
	s.failed = true
	s.status = status
	return nil
}

type closeErrorReader struct {
	io.Reader
}

func (closeErrorReader) Close() error { return errors.New("synthetic close error") }

type workerTestBlobs struct {
	reader io.ReadCloser
}

func (b workerTestBlobs) OpenStream(context.Context, string) (io.ReadCloser, int64, error) {
	return b.reader, 4, nil
}

type workerTestExtractor struct {
	response Response
	err      error
}

func (e workerTestExtractor) Extract(context.Context, string, string, int64, io.Reader) (Response, error) {
	return e.response, e.err
}

func TestWorkerKeepsSuccessfulExtractionWhenReaderCloseFails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	workStore := &workerTestStore{}
	worker := NewWorker(workStore,
		workerTestBlobs{reader: closeErrorReader{Reader: io.LimitReader(&zeroReader{}, 4)}},
		workerTestExtractor{response: Response{Method: "ocr", Pages: []store.OCRPage{{PageNumber: 1, Method: "ocr", Text: "text"}}}},
		WorkerConfig{Fingerprint: "v1"}, slog.Default())

	err := worker.process(t.Context(), &store.OCRJob{ContentHash: "hash", Attempts: 1})
	requirements.NoError(err)
	assertions.True(workStore.completed)
	assertions.False(workStore.failed)
}

func TestWorkerRecordsRetryExhaustionSeparatelyFromUnsupported(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	workStore := &workerTestStore{}
	worker := NewWorker(workStore, workerTestBlobs{}, workerTestExtractor{},
		WorkerConfig{Fingerprint: "v1", MaxAttempts: 3}, slog.Default())

	err := worker.recordFailure(t.Context(), &store.OCRJob{ContentHash: "hash", Attempts: 3},
		"executor_unavailable", "temporary failure", false)
	requirements.Error(err)
	assertions.True(workStore.failed)
	assertions.Equal(store.OCRExhausted, workStore.status)
}

type zeroReader struct{}

func (*zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

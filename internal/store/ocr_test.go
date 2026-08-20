package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestOCRLifecycleDeduplicatesLeasesAndSearchesPages(t *testing.T) {
	f := storetest.New(t)
	hash := strings.Repeat("ab", 32)
	first := f.CreateMessage("ocr-first")
	second := f.CreateMessage("ocr-second")
	require.NoError(t, f.Store.UpsertAttachment(first, "invoice.pdf", "application/pdf", "ab/one", hash, 100))
	require.NoError(t, f.Store.UpsertAttachment(second, "copy.pdf", "application/pdf", "ab/two", hash, 100))

	discovered, err := f.Store.EnqueueOCRBacklog(t.Context(), "extractor-v1", 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), discovered, "duplicate attachment bytes produce one OCR job")

	job, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, hash, job.ContentHash)
	assert.Equal(t, 1, job.Attempts)

	secondClaim, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	require.NoError(t, err)
	assert.Nil(t, secondClaim, "active lease prevents duplicate extraction")

	pages := []store.OCRPage{
		{PageNumber: 1, Method: "native", Text: "Synthetic invoice reference ORCHID-417", Confidence: 100},
		{PageNumber: 2, Method: "ocr", Text: "Synthetic total 42 euros", Confidence: 88},
	}
	require.NoError(t, f.Store.CompleteOCR(t.Context(), hash, "extractor-v1", "mixed", pages))

	result, err := f.Store.GetOCRResult(t.Context(), hash, true)
	require.NoError(t, err)
	assert.Equal(t, store.OCRReady, result.Status)
	assert.Equal(t, "mixed", result.Method)
	assert.Equal(t, 94.0, result.AverageConfidence)
	assert.Equal(t, pages, result.Pages)

	hits, err := f.Store.SearchOCR(t.Context(), "ORCHID", 20)
	require.NoError(t, err)
	require.Len(t, hits, 2, "each parent attachment remains discoverable")
	assert.Equal(t, 1, hits[0].PageNumber)
	assert.Contains(t, hits[0].Snippet, "ORCHID")
	assert.NotEqual(t, hits[0].MessageID, hits[1].MessageID)
}

func TestOCRFingerprintUpgradeRequeuesReadyResult(t *testing.T) {
	f := storetest.New(t)
	hash := strings.Repeat("cd", 32)
	messageID := f.CreateMessage("ocr-upgrade")
	require.NoError(t, f.Store.UpsertAttachment(messageID, "scan.png", "image/png", "cd/scan", hash, 50))
	_, err := f.Store.RequestOCR(t.Context(), hash, "extractor-v1")
	require.NoError(t, err)
	job, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.NoError(t, f.Store.CompleteOCR(t.Context(), hash, "extractor-v1", "ocr",
		[]store.OCRPage{{PageNumber: 1, Method: "ocr", Text: "old text", Confidence: 80}}))

	discovered, err := f.Store.EnqueueOCRBacklog(t.Context(), "extractor-v2", 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), discovered)
	upgraded, err := f.Store.GetOCRResult(t.Context(), hash, true)
	require.NoError(t, err)
	assert.Equal(t, store.OCRPending, upgraded.Status)
	assert.Equal(t, "extractor-v2", upgraded.Fingerprint)
	hits, err := f.Store.SearchOCR(t.Context(), "old", 20)
	require.NoError(t, err)
	assert.Empty(t, hits, "stale pages are hidden while the new fingerprint is pending")
}

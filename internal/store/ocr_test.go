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
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	hash := strings.Repeat("ab", 32)
	first := f.CreateMessage("ocr-first")
	second := f.CreateMessage("ocr-second")
	requirements.NoError(f.Store.UpsertAttachment(first, "invoice.pdf", "application/pdf", "ab/one", hash, 100))
	requirements.NoError(f.Store.UpsertAttachment(second, "copy.pdf", "application/pdf", "ab/two", hash, 100))

	discovered, err := f.Store.EnqueueOCRBacklog(t.Context(), "extractor-v1", 10)
	requirements.NoError(err)
	assertions.Equal(int64(1), discovered, "duplicate attachment bytes produce one OCR job")

	job, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	requirements.NoError(err)
	requirements.NotNil(job)
	assertions.Equal(hash, job.ContentHash)
	assertions.Equal(1, job.Attempts)

	secondClaim, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	requirements.NoError(err)
	assertions.Nil(secondClaim, "active lease prevents duplicate extraction")

	pages := []store.OCRPage{
		{PageNumber: 1, Method: "native", Text: "Synthetic invoice reference ORCHID-417", Confidence: 100},
		{PageNumber: 2, Method: "ocr", Text: "Synthetic total 42 euros", Confidence: 88},
	}
	requirements.NoError(f.Store.CompleteOCR(t.Context(), hash, "extractor-v1", job.Attempts, "mixed", pages))

	result, err := f.Store.GetOCRResult(t.Context(), hash, true)
	requirements.NoError(err)
	assertions.Equal(store.OCRReady, result.Status)
	assertions.Equal("mixed", result.Method)
	assertions.Equal(94.0, result.AverageConfidence)
	assertions.Equal(pages, result.Pages)

	hits, err := f.Store.SearchOCR(t.Context(), "ORCHID", 20)
	requirements.NoError(err)
	requirements.Len(hits, 2, "each parent attachment remains discoverable")
	assertions.Equal(1, hits[0].PageNumber)
	assertions.Contains(hits[0].Snippet, "ORCHID")
	assertions.NotEqual(hits[0].MessageID, hits[1].MessageID)
}

func TestOCRFingerprintUpgradeRequeuesReadyResult(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	hash := strings.Repeat("cd", 32)
	messageID := f.CreateMessage("ocr-upgrade")
	requirements.NoError(f.Store.UpsertAttachment(messageID, "scan.png", "image/png", "cd/scan", hash, 50))
	_, err := f.Store.RequestOCR(t.Context(), hash, "extractor-v1")
	requirements.NoError(err)
	job, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	requirements.NoError(err)
	requirements.NotNil(job)
	requirements.NoError(f.Store.CompleteOCR(t.Context(), hash, "extractor-v1", job.Attempts, "ocr",
		[]store.OCRPage{{PageNumber: 1, Method: "ocr", Text: "old text", Confidence: 80}}))

	discovered, err := f.Store.EnqueueOCRBacklog(t.Context(), "extractor-v2", 10)
	requirements.NoError(err)
	assertions.Equal(int64(1), discovered)
	upgraded, err := f.Store.GetOCRResult(t.Context(), hash, true)
	requirements.NoError(err)
	assertions.Equal(store.OCRPending, upgraded.Status)
	assertions.Equal("extractor-v2", upgraded.Fingerprint)
	hits, err := f.Store.SearchOCR(t.Context(), "old", 20)
	requirements.NoError(err)
	assertions.Empty(hits, "stale pages are hidden while the new fingerprint is pending")
}

func TestOCRFingerprintUpgradeWaitsForRunningLease(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	hash := strings.Repeat("de", 32)
	messageID := f.CreateMessage("ocr-running-upgrade")
	requirements.NoError(f.Store.UpsertAttachment(messageID, "scan.png", "image/png", "de/scan", hash, 50))
	_, err := f.Store.RequestOCR(t.Context(), hash, "extractor-v1")
	requirements.NoError(err)
	job, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	requirements.NoError(err)
	requirements.NotNil(job)

	discovered, err := f.Store.EnqueueOCRBacklog(t.Context(), "extractor-v2", 10)
	requirements.NoError(err)
	assertions.Zero(discovered, "running fingerprint must not be replaced")
	running, err := f.Store.GetOCRResult(t.Context(), hash, false)
	requirements.NoError(err)
	assertions.Equal(store.OCRRunning, running.Status)
	assertions.Equal("extractor-v1", running.Fingerprint)
	requirements.NoError(f.Store.CompleteOCR(t.Context(), hash, "extractor-v1", job.Attempts, "ocr",
		[]store.OCRPage{{PageNumber: 1, Method: "ocr", Text: "v1 result", Confidence: 80}}))

	discovered, err = f.Store.EnqueueOCRBacklog(t.Context(), "extractor-v2", 10)
	requirements.NoError(err)
	assertions.Equal(int64(1), discovered)
}

func TestOCRClaimUsesMetadataFromOneAttachmentRow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	hash := strings.Repeat("ef", 32)
	first := f.CreateMessage("ocr-metadata-first")
	second := f.CreateMessage("ocr-metadata-second")
	requirements.NoError(f.Store.UpsertAttachment(first, "zeta.pdf", "application/pdf", "ef/first", hash, 111))
	requirements.NoError(f.Store.UpsertAttachment(second, "alpha.png", "image/png", "ef/second", hash, 222))
	_, err := f.Store.EnqueueOCRBacklog(t.Context(), "extractor-v1", 10)
	requirements.NoError(err)

	job, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	requirements.NoError(err)
	requirements.NotNil(job)
	assertions.Equal("zeta.pdf", job.Filename)
	assertions.Equal("application/pdf", job.MIMEType)
	assertions.Equal(int64(111), job.Size)
}

func TestOCRStaleWorkerCannotCompleteReclaimedLease(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	hash := strings.Repeat("fa", 32)
	messageID := f.CreateMessage("ocr-reclaimed")
	requirements.NoError(f.Store.UpsertAttachment(messageID, "scan.png", "image/png", "fa/scan", hash, 50))
	_, err := f.Store.RequestOCR(t.Context(), hash, "extractor-v1")
	requirements.NoError(err)
	stale, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	requirements.NoError(err)
	requirements.NotNil(stale)
	_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE attachment_ocr SET lease_expires_at = ? WHERE content_hash = ?`, time.Now().Add(-time.Minute), hash)
	requirements.NoError(err)
	current, err := f.Store.ClaimOCRJob(t.Context(), "extractor-v1", time.Minute)
	requirements.NoError(err)
	requirements.NotNil(current)

	err = f.Store.CompleteOCR(t.Context(), hash, "extractor-v1", stale.Attempts, "ocr",
		[]store.OCRPage{{PageNumber: 1, Method: "ocr", Text: "stale", Confidence: 50}})
	requirements.ErrorIs(err, store.ErrOCRLeaseLost)
	requirements.NoError(f.Store.CompleteOCR(t.Context(), hash, "extractor-v1", current.Attempts, "ocr",
		[]store.OCRPage{{PageNumber: 1, Method: "ocr", Text: "current", Confidence: 90}}))
	result, err := f.Store.GetOCRResult(t.Context(), hash, true)
	requirements.NoError(err)
	requirements.Len(result.Pages, 1)
	assertions.Equal("current", result.Pages[0].Text)
}

func TestOCRSearchReportsUnavailableFTS(t *testing.T) {
	f := storetest.New(t)
	store.SetFTS5AvailableForTest(f.Store, false)
	_, err := f.Store.SearchOCR(t.Context(), "invoice", 20)
	require.ErrorIs(t, err, store.ErrOCRSearchUnavailable)
}

func TestOCRCandidateIndexExists(t *testing.T) {
	f := storetest.New(t)
	var name string
	err := f.Store.DB().QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_attachments_ocr_candidates'`).Scan(&name)
	require.NoError(t, err)
	assert.Equal(t, "idx_attachments_ocr_candidates", name)
}

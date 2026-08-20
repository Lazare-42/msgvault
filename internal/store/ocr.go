package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	OCRPending     = "pending"
	OCRRunning     = "running"
	OCRReady       = "ready"
	OCRFailed      = "failed"
	OCRExhausted   = "exhausted"
	OCRUnsupported = "unsupported"
)

var (
	ErrOCRLeaseLost         = errors.New("OCR lease lost")
	ErrOCRSearchUnavailable = errors.New("OCR full-text search is unavailable")
)

type OCRJob struct {
	ContentHash string
	Filename    string
	MIMEType    string
	Size        int64
	Attempts    int
}

type OCRPage struct {
	PageNumber int     `json:"page_number"`
	Method     string  `json:"method"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence,omitempty"`
}

type OCRResult struct {
	ContentHash       string    `json:"content_hash"`
	Status            string    `json:"status"`
	Fingerprint       string    `json:"extractor_fingerprint"`
	Method            string    `json:"method,omitempty"`
	PageCount         int       `json:"page_count,omitempty"`
	AverageConfidence float64   `json:"average_confidence,omitempty"`
	Attempts          int       `json:"attempts"`
	ErrorCode         string    `json:"error_code,omitempty"`
	ErrorDetail       string    `json:"error_detail,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
	Pages             []OCRPage `json:"pages,omitempty"`
}

type OCRSearchHit struct {
	AttachmentID   int64   `json:"attachment_id"`
	ContentHash    string  `json:"content_hash"`
	Filename       string  `json:"filename"`
	MIMEType       string  `json:"mime_type"`
	PageNumber     int     `json:"page_number"`
	Method         string  `json:"method"`
	Confidence     float64 `json:"confidence,omitempty"`
	Snippet        string  `json:"snippet"`
	MessageID      int64   `json:"message_id"`
	ConversationID int64   `json:"conversation_id"`
}

type OCRSummary struct {
	Pending     int64 `json:"pending"`
	Running     int64 `json:"running"`
	Ready       int64 `json:"ready"`
	Failed      int64 `json:"failed"`
	Exhausted   int64 `json:"exhausted"`
	Unsupported int64 `json:"unsupported"`
}

type OCRRuntimeStatus struct {
	Enabled     bool   `json:"enabled"`
	Fingerprint string `json:"extractor_fingerprint"`
	OCRSummary
}

func (s *Store) OCRSummary(ctx context.Context) (OCRSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM attachment_ocr GROUP BY status`)
	if err != nil {
		return OCRSummary{}, fmt.Errorf("summarize OCR: %w", err)
	}
	defer rows.Close()
	var out OCRSummary
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return out, err
		}
		switch status {
		case OCRPending:
			out.Pending = count
		case OCRRunning:
			out.Running = count
		case OCRReady:
			out.Ready = count
		case OCRFailed:
			out.Failed = count
		case OCRExhausted:
			out.Exhausted = count
		case OCRUnsupported:
			out.Unsupported = count
		}
	}
	return out, rows.Err()
}

// EnqueueOCRBacklog discovers supported CAS blobs through attachment rows.
// It never walks storage paths, and resets rows when the extractor fingerprint
// changes so upgrades are naturally incremental.
func (s *Store) EnqueueOCRBacklog(ctx context.Context, fingerprint string, limit int) (int64, error) {
	if limit <= 0 {
		limit = 100
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO attachment_ocr (content_hash, status, extractor_fingerprint)
		SELECT LOWER(a.content_hash), 'pending', ?
		FROM attachments a
		WHERE a.content_hash IS NOT NULL AND a.content_hash <> ''
		  AND (LOWER(COALESCE(a.mime_type, '')) = 'application/pdf'
		       OR LOWER(COALESCE(a.mime_type, '')) LIKE 'image/%'
		       OR LOWER(COALESCE(a.filename, '')) LIKE '%.pdf')
		  AND NOT EXISTS (
		      SELECT 1 FROM attachment_ocr o
		      WHERE o.content_hash = LOWER(a.content_hash)
		        AND o.extractor_fingerprint = ?)
		GROUP BY LOWER(a.content_hash)
		LIMIT ?
		ON CONFLICT(content_hash) DO UPDATE SET
		  status = 'pending', extractor_fingerprint = excluded.extractor_fingerprint,
		  priority = 0, attempts = 0, lease_expires_at = NULL,
		  next_attempt_at = NULL, error_code = NULL, error_detail = NULL,
		  updated_at = CURRENT_TIMESTAMP, completed_at = NULL
		WHERE attachment_ocr.extractor_fingerprint <> excluded.extractor_fingerprint
		  AND attachment_ocr.status <> 'running'`,
		fingerprint, fingerprint, limit)
	if err != nil {
		return 0, fmt.Errorf("enqueue OCR backlog: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("OCR backlog rows affected: %w", err)
	}
	return n, nil
}

func (s *Store) RequestOCR(ctx context.Context, contentHash, fingerprint string) (*OCRResult, error) {
	contentHash = strings.ToLower(strings.TrimSpace(contentHash))
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE LOWER(content_hash) = ?`, contentHash).Scan(&exists); err != nil {
		return nil, fmt.Errorf("find OCR attachment: %w", err)
	}
	if exists == 0 {
		return nil, sql.ErrNoRows
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO attachment_ocr (content_hash, status, extractor_fingerprint, priority)
		VALUES (?, 'pending', ?, 100)
		ON CONFLICT(content_hash) DO UPDATE SET
		  priority = CASE WHEN attachment_ocr.priority > 100 THEN attachment_ocr.priority ELSE 100 END,
		  status = CASE WHEN attachment_ocr.status = 'running' THEN 'running'
		                WHEN attachment_ocr.status = 'ready'
		                 AND attachment_ocr.extractor_fingerprint = excluded.extractor_fingerprint
		                THEN 'ready' ELSE 'pending' END,
		  extractor_fingerprint = CASE WHEN attachment_ocr.status = 'running'
		                               THEN attachment_ocr.extractor_fingerprint
		                               ELSE excluded.extractor_fingerprint END,
		  attempts = CASE WHEN attachment_ocr.status = 'running'
		                       OR (attachment_ocr.status = 'ready'
		                           AND attachment_ocr.extractor_fingerprint = excluded.extractor_fingerprint)
		                  THEN attachment_ocr.attempts ELSE 0 END,
		  next_attempt_at = CASE WHEN attachment_ocr.status = 'running'
		                         THEN attachment_ocr.next_attempt_at ELSE NULL END,
		  error_code = CASE WHEN attachment_ocr.status = 'running'
		                    THEN attachment_ocr.error_code ELSE NULL END,
		  error_detail = CASE WHEN attachment_ocr.status = 'running'
		                      THEN attachment_ocr.error_detail ELSE NULL END,
		  completed_at = CASE WHEN attachment_ocr.status = 'running'
		                      OR (attachment_ocr.status = 'ready'
		                          AND attachment_ocr.extractor_fingerprint = excluded.extractor_fingerprint)
		                 THEN attachment_ocr.completed_at ELSE NULL END,
		  updated_at = CURRENT_TIMESTAMP`, contentHash, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("request OCR: %w", err)
	}
	return s.GetOCRResult(ctx, contentHash, false)
}

func (s *Store) ClaimOCRJob(ctx context.Context, fingerprint string, lease time.Duration) (*OCRJob, error) {
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin OCR claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var job OCRJob
	now := time.Now().UTC()
	err = tx.QueryRowContext(ctx, `
		SELECT o.content_hash, COALESCE(a.filename, ''),
		       COALESCE(a.mime_type, ''), COALESCE(a.size, 0), o.attempts
		FROM attachment_ocr o
		JOIN attachments a ON a.id = (
		    SELECT MIN(a2.id) FROM attachments a2
		    WHERE LOWER(a2.content_hash) = o.content_hash)
		WHERE o.extractor_fingerprint = ?
		  AND ((o.status IN ('pending', 'failed') AND
		        (o.next_attempt_at IS NULL OR o.next_attempt_at <= ?))
		       OR (o.status = 'running' AND o.lease_expires_at < ?))
		ORDER BY o.priority DESC, o.created_at, o.content_hash
		LIMIT 1`, fingerprint, now, now).Scan(&job.ContentHash, &job.Filename, &job.MIMEType, &job.Size, &job.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select OCR claim: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE attachment_ocr SET status = 'running', attempts = attempts + 1,
		 lease_expires_at = ?, updated_at = ?
		WHERE content_hash = ? AND extractor_fingerprint = ?
		  AND (status <> 'running' OR lease_expires_at < ?)`,
		now.Add(lease), now, job.ContentHash, fingerprint, now)
	if err != nil {
		return nil, fmt.Errorf("update OCR claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return nil, fmt.Errorf("OCR claim lost")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit OCR claim: %w", err)
	}
	job.Attempts++
	return &job, nil
}

func (s *Store) CompleteOCR(ctx context.Context, hash, fingerprint string, attempt int, method string, pages []OCRPage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin complete OCR: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachment_ocr_pages WHERE content_hash = ?`, hash); err != nil {
		return fmt.Errorf("clear OCR pages: %w", err)
	}
	if s.dialect.DriverName() != "pgx" && s.fts5Available {
		if _, err := tx.ExecContext(ctx, `DELETE FROM attachment_ocr_fts WHERE content_hash = ?`, hash); err != nil {
			return fmt.Errorf("clear OCR FTS: %w", err)
		}
	}
	var confidence float64
	for _, page := range pages {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachment_ocr_pages
			(content_hash, page_number, method, text, confidence) VALUES (?, ?, ?, ?, ?)`,
			hash, page.PageNumber, page.Method, page.Text, page.Confidence); err != nil {
			return fmt.Errorf("insert OCR page %d: %w", page.PageNumber, err)
		}
		if s.dialect.DriverName() != "pgx" && s.fts5Available {
			if _, err := tx.ExecContext(ctx, `INSERT INTO attachment_ocr_fts
				(content_hash, page_number, text) VALUES (?, ?, ?)`, hash, page.PageNumber, page.Text); err != nil {
				return fmt.Errorf("index OCR page %d: %w", page.PageNumber, err)
			}
		}
		confidence += page.Confidence
	}
	if len(pages) > 0 {
		confidence /= float64(len(pages))
	}
	res, err := tx.ExecContext(ctx, `UPDATE attachment_ocr SET status = 'ready', method = ?,
		page_count = ?, average_confidence = ?, priority = 0, lease_expires_at = NULL,
		next_attempt_at = NULL, error_code = NULL, error_detail = NULL,
		updated_at = CURRENT_TIMESTAMP, completed_at = CURRENT_TIMESTAMP
		WHERE content_hash = ? AND extractor_fingerprint = ? AND status = 'running' AND attempts = ?`,
		method, len(pages), confidence, hash, fingerprint, attempt)
	if err != nil {
		return fmt.Errorf("complete OCR row: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("read OCR completion rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w during completion for %s", ErrOCRLeaseLost, hash)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit OCR completion: %w", err)
	}
	return nil
}

func (s *Store) FailOCR(ctx context.Context, hash, fingerprint string, attempt int, code, detail, status string, backoff time.Duration) error {
	if status != OCRFailed && status != OCRUnsupported && status != OCRExhausted {
		return fmt.Errorf("invalid OCR failure status %q", status)
	}
	if backoff <= 0 {
		backoff = 5 * time.Minute
	}
	var retryAt any = time.Now().UTC().Add(backoff)
	if status != OCRFailed {
		retryAt = nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE attachment_ocr SET status = ?, error_code = ?,
		error_detail = ?, lease_expires_at = NULL, next_attempt_at = ?,
		updated_at = CURRENT_TIMESTAMP WHERE content_hash = ? AND extractor_fingerprint = ?
		AND status = 'running' AND attempts = ?`,
		status, code, detail, retryAt, hash, fingerprint, attempt)
	if err != nil {
		return fmt.Errorf("fail OCR: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("read OCR failure rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w while recording failure for %s", ErrOCRLeaseLost, hash)
	}
	return nil
}

func (s *Store) GetOCRResult(ctx context.Context, hash string, includePages bool) (*OCRResult, error) {
	var out OCRResult
	err := s.db.QueryRowContext(ctx, `SELECT content_hash, status, extractor_fingerprint,
		COALESCE(method, ''), COALESCE(page_count, 0), COALESCE(average_confidence, 0),
		attempts, COALESCE(error_code, ''), COALESCE(error_detail, ''), updated_at
		FROM attachment_ocr WHERE content_hash = ?`, strings.ToLower(hash)).Scan(
		&out.ContentHash, &out.Status, &out.Fingerprint, &out.Method, &out.PageCount,
		&out.AverageConfidence, &out.Attempts, &out.ErrorCode, &out.ErrorDetail, &out.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if !includePages {
		return &out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT page_number, method, text, COALESCE(confidence, 0)
		FROM attachment_ocr_pages WHERE content_hash = ? ORDER BY page_number`, strings.ToLower(hash))
	if err != nil {
		return nil, fmt.Errorf("list OCR pages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var page OCRPage
		if err := rows.Scan(&page.PageNumber, &page.Method, &page.Text, &page.Confidence); err != nil {
			return nil, fmt.Errorf("scan OCR page: %w", err)
		}
		out.Pages = append(out.Pages, page)
	}
	return &out, rows.Err()
}

func (s *Store) SearchOCR(ctx context.Context, query string, limit int) ([]OCRSearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("OCR search query is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	searchArg := s.dialect.BuildFTSArg(strings.Fields(query))
	if searchArg == "" {
		return nil, errors.New("OCR search query has no searchable terms")
	}
	if s.dialect.DriverName() != "pgx" && !s.fts5Available {
		return nil, ErrOCRSearchUnavailable
	}
	var statement string
	if s.dialect.DriverName() == "pgx" {
		statement = `SELECT a.id, p.content_hash, COALESCE(a.filename, ''), COALESCE(a.mime_type, ''),
			p.page_number, p.method, COALESCE(p.confidence, 0),
			ts_headline('simple', p.text, to_tsquery('simple', ?)), a.message_id, m.conversation_id
			FROM attachment_ocr_pages p JOIN attachment_ocr o ON o.content_hash = p.content_hash AND o.status = 'ready'
			JOIN attachments a ON LOWER(a.content_hash) = p.content_hash
			JOIN messages m ON m.id = a.message_id
			WHERE p.search_fts @@ to_tsquery('simple', ?)
			ORDER BY ts_rank(p.search_fts, to_tsquery('simple', ?)) DESC, a.id LIMIT ?`
	} else {
		statement = `SELECT a.id, f.content_hash, COALESCE(a.filename, ''), COALESCE(a.mime_type, ''),
			CAST(f.page_number AS INTEGER), p.method, COALESCE(p.confidence, 0),
			snippet(attachment_ocr_fts, 2, '[', ']', '…', 24), a.message_id, m.conversation_id
			FROM attachment_ocr_fts f JOIN attachment_ocr o ON o.content_hash = f.content_hash AND o.status = 'ready'
			JOIN attachment_ocr_pages p
			  ON p.content_hash = f.content_hash AND p.page_number = CAST(f.page_number AS INTEGER)
			JOIN attachments a ON LOWER(a.content_hash) = f.content_hash
			JOIN messages m ON m.id = a.message_id
			WHERE attachment_ocr_fts MATCH ? ORDER BY bm25(attachment_ocr_fts), a.id LIMIT ?`
	}
	args := []any{searchArg, limit}
	if s.dialect.DriverName() == "pgx" {
		args = []any{searchArg, searchArg, searchArg, limit}
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("search OCR text: %w", err)
	}
	defer rows.Close()
	var hits []OCRSearchHit
	for rows.Next() {
		var hit OCRSearchHit
		if err := rows.Scan(&hit.AttachmentID, &hit.ContentHash, &hit.Filename, &hit.MIMEType,
			&hit.PageNumber, &hit.Method, &hit.Confidence, &hit.Snippet,
			&hit.MessageID, &hit.ConversationID); err != nil {
			return nil, fmt.Errorf("scan OCR hit: %w", err)
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

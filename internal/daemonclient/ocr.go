package daemonclient

import (
	"context"
	"database/sql"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) SearchOCR(ctx context.Context, query string, limit int) ([]store.OCRSearchHit, error) {
	body := generated.OcrSearchRequest{Query: query}
	if limit > 0 {
		value := int64(limit)
		body.Limit = &value
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchAttachmentTextResp, error) {
		return client.SearchAttachmentTextWithResponse(ctx, &generated.SearchAttachmentTextRequestOptions{Body: &body})
	})
	if err != nil || resp.JSON200 == nil {
		return nil, err
	}
	hits := make([]store.OCRSearchHit, len(resp.JSON200.Results))
	for i, hit := range resp.JSON200.Results {
		hits[i] = store.OCRSearchHit{
			AttachmentID: hit.AttachmentID, ContentHash: hit.ContentHash,
			Filename: hit.Filename, MIMEType: hit.MimeType, PageNumber: int(hit.PageNumber),
			Method: hit.Method, Confidence: ocrFloat64Value(hit.Confidence), Snippet: hit.Snippet,
			MessageID: hit.MessageID, ConversationID: hit.ConversationID,
		}
	}
	return hits, nil
}

func (c *Client) OCRStatus(ctx context.Context) (*store.OCRRuntimeStatus, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetOCRStatusResp, error) {
		return client.GetOCRStatusWithResponse(ctx)
	})
	if err != nil || resp.JSON200 == nil {
		return nil, err
	}
	return &store.OCRRuntimeStatus{
		Enabled: resp.JSON200.Enabled, Fingerprint: resp.JSON200.ExtractorFingerprint,
		OCRSummary: store.OCRSummary{
			Pending: resp.JSON200.Pending, Running: resp.JSON200.Running,
			Ready: resp.JSON200.Ready, Failed: resp.JSON200.Failed,
			Exhausted: resp.JSON200.Exhausted, Unsupported: resp.JSON200.Unsupported,
		},
	}, nil
}

func (c *Client) GetOCRResult(ctx context.Context, hash string, _ bool) (*store.OCRResult, error) {
	resp, err := APIResponseWithNotFound(c, func(client *apiclient.Client) (*generated.GetAttachmentTextResp, error) {
		return client.GetAttachmentTextWithResponse(ctx, &generated.GetAttachmentTextRequestOptions{
			PathParams: &generated.GetAttachmentTextPath{Hash: hash},
		})
	}, func(*generated.GetAttachmentTextResp) error { return sql.ErrNoRows })
	if err != nil || resp.JSON200 == nil {
		return nil, err
	}
	return ocrResultFromGenerated(resp.JSON200), nil
}

func (c *Client) RequestOCR(ctx context.Context, hash, _ string) (*store.OCRResult, error) {
	resp, err := APIResponseWithStatuses(c, []int{202}, func(client *apiclient.Client) (*generated.RequestAttachmentTextResp, error) {
		return client.RequestAttachmentTextWithResponse(ctx, &generated.RequestAttachmentTextRequestOptions{
			PathParams: &generated.RequestAttachmentTextPath{Hash: hash},
		})
	})
	if err != nil || resp.JSON202 == nil {
		return nil, err
	}
	return ocrResultFromGenerated(resp.JSON202), nil
}

func ocrResultFromGenerated(result *generated.OCRResult) *store.OCRResult {
	if result == nil {
		return nil
	}
	out := &store.OCRResult{
		ContentHash: result.ContentHash, Status: result.Status,
		Fingerprint: result.ExtractorFingerprint, Method: stringValue(result.Method),
		PageCount: intValue(result.PageCount), AverageConfidence: ocrFloat64Value(result.AverageConfidence),
		Attempts: int(result.Attempts), ErrorCode: stringValue(result.ErrorCode),
		ErrorDetail: stringValue(result.ErrorDetail), UpdatedAt: result.UpdatedAt,
		Pages: make([]store.OCRPage, len(result.Pages)),
	}
	for i, page := range result.Pages {
		out.Pages[i] = store.OCRPage{
			PageNumber: int(page.PageNumber), Method: page.Method,
			Text: page.Text, Confidence: ocrFloat64Value(page.Confidence),
		}
	}
	return out
}

func ocrFloat64Value(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

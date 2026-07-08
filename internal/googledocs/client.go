// Package googledocs wraps Google Drive and Docs APIs for configured
// folder-scoped Google Docs sources.
package googledocs

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/config"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

const (
	GoogleDocMimeType = "application/vnd.google-apps.document"
	PlainTextMimeType = "text/plain"
)

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

var driveIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// File is a Google Doc metadata summary returned to MCP clients.
type File struct {
	Source       string `json:"source"`
	DocumentID   string `json:"document_id"`
	Name         string `json:"name"`
	ModifiedTime string `json:"modified_time,omitempty"`
	WebViewLink  string `json:"web_view_link,omitempty"`
}

// Document is a plain-text export of a Google Doc plus metadata.
type Document struct {
	File
	Text          string `json:"text"`
	TextLength    int    `json:"text_length"`
	TextTruncated bool   `json:"text_truncated"`
}

// AppendResult reports the outcome of appending text to a Google Doc.
type AppendResult struct {
	Source        string `json:"source"`
	DocumentID    string `json:"document_id"`
	InsertedChars int    `json:"inserted_chars"`
	Status        string `json:"status"`
}

// ReplaceResult reports the outcome of replacing text in a Google Doc.
type ReplaceResult struct {
	Source             string `json:"source"`
	DocumentID         string `json:"document_id"`
	OccurrencesChanged int64  `json:"occurrences_changed"`
	Status             string `json:"status"`
}

// Client is the Google Docs capability exposed through MCP.
type Client interface {
	ListDocs(ctx context.Context, sourceName, query string, limit int) ([]File, error)
	GetDoc(ctx context.Context, sourceName, documentID string, maxChars int) (*Document, error)
	AppendText(ctx context.Context, sourceName, documentID, text string) (*AppendResult, error)
	ReplaceText(ctx context.Context, sourceName, documentID, find, replacement string, matchCase bool) (*ReplaceResult, error)
}

// SourceServices binds one configured source to authenticated Drive/Docs services.
type SourceServices struct {
	Source config.GoogleDocsSource
	Drive  *drive.Service
	Docs   *docs.Service
}

type sourceRuntime struct {
	source config.GoogleDocsSource
	drive  *drive.Service
	docs   *docs.Service
}

// LiveClient uses authenticated Google API services to access configured Docs.
type LiveClient struct {
	sources map[string]sourceRuntime
	order   []string
}

// NewClient creates a folder-scoped Google Docs client.
func NewClient(sources []SourceServices) (*LiveClient, error) {
	c := &LiveClient{
		sources: make(map[string]sourceRuntime, len(sources)),
		order:   make([]string, 0, len(sources)),
	}
	for _, services := range sources {
		src := services.Source
		if !src.Enabled {
			continue
		}
		if err := ValidateSource(src); err != nil {
			return nil, err
		}
		if services.Drive == nil {
			return nil, fmt.Errorf("google-docs source %q Drive service is nil", src.Name)
		}
		if services.Docs == nil {
			return nil, fmt.Errorf("google-docs source %q Docs service is nil", src.Name)
		}
		if _, exists := c.sources[src.Name]; exists {
			return nil, fmt.Errorf("duplicate google-docs source %q", src.Name)
		}
		c.sources[src.Name] = sourceRuntime{
			source: src,
			drive:  services.Drive,
			docs:   services.Docs,
		}
		c.order = append(c.order, src.Name)
	}
	return c, nil
}

// ValidateSource checks the required fields for a Google Docs source.
func ValidateSource(src config.GoogleDocsSource) error {
	if strings.TrimSpace(src.Name) == "" {
		return fmt.Errorf("google-docs source name is required")
	}
	if strings.TrimSpace(src.GoogleAccount) == "" {
		return fmt.Errorf("google-docs source %q google_account is required", src.Name)
	}
	if strings.TrimSpace(src.FolderID) == "" {
		return fmt.Errorf("google-docs source %q folder_id is required", src.Name)
	}
	if !driveIDPattern.MatchString(src.FolderID) {
		return fmt.Errorf("google-docs source %q folder_id is invalid", src.Name)
	}
	return nil
}

func (c *LiveClient) resolveSource(sourceName string) (sourceRuntime, error) {
	if c == nil || len(c.order) == 0 {
		return sourceRuntime{}, fmt.Errorf("no Google Docs sources configured")
	}
	sourceName = strings.TrimSpace(sourceName)
	if sourceName == "" {
		if len(c.order) == 1 {
			return c.sources[c.order[0]], nil
		}
		return sourceRuntime{}, fmt.Errorf("multiple Google Docs sources configured; specify source")
	}
	src, ok := c.sources[sourceName]
	if !ok {
		return sourceRuntime{}, fmt.Errorf("Google Docs source %q not configured or disabled", sourceName)
	}
	return src, nil
}

// ListDocs lists Google Docs directly in the configured Drive folder.
func (c *LiveClient) ListDocs(ctx context.Context, sourceName, query string, limit int) ([]File, error) {
	src, err := c.resolveSource(sourceName)
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit)
	q := driveDocsQuery(src.source.FolderID, query)

	out := make([]File, 0, limit)
	pageToken := ""
	for len(out) < limit {
		pageSize := min(maxListLimit, limit-len(out))
		call := src.drive.Files.List().
			Context(ctx).
			Q(q).
			PageSize(int64(pageSize)).
			OrderBy("modifiedTime desc,name").
			Fields(googleapi.Field("nextPageToken,files(id,name,mimeType,modifiedTime,webViewLink,parents)")).
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list Drive docs: %w", err)
		}
		for _, f := range resp.Files {
			if f.MimeType != GoogleDocMimeType {
				continue
			}
			out = append(out, fileFromDrive(src.source.Name, f))
			if len(out) >= limit {
				break
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

// GetDoc exports a folder-scoped Google Doc as plain text.
func (c *LiveClient) GetDoc(ctx context.Context, sourceName, documentID string, maxChars int) (*Document, error) {
	src, file, err := c.getDocFile(ctx, sourceName, documentID)
	if err != nil {
		return nil, err
	}
	resp, err := src.drive.Files.Export(file.DocumentID, PlainTextMimeType).Context(ctx).Download()
	if err != nil {
		return nil, fmt.Errorf("export Google Doc: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read exported Google Doc: %w", err)
	}
	text := string(body)
	textLen := utf8.RuneCountInString(text)
	truncated := false
	if maxChars > 0 && textLen > maxChars {
		text = truncateRunes(text, maxChars)
		truncated = true
	}
	return &Document{
		File:          file,
		Text:          text,
		TextLength:    textLen,
		TextTruncated: truncated,
	}, nil
}

// AppendText appends text to a folder-scoped Google Doc.
func (c *LiveClient) AppendText(ctx context.Context, sourceName, documentID, text string) (*AppendResult, error) {
	src, file, err := c.getDocFile(ctx, sourceName, documentID)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, fmt.Errorf("text is required")
	}
	_, err = src.docs.Documents.BatchUpdate(file.DocumentID, &docs.BatchUpdateDocumentRequest{
		Requests: []*docs.Request{{
			InsertText: &docs.InsertTextRequest{
				EndOfSegmentLocation: &docs.EndOfSegmentLocation{},
				Text:                 text,
			},
		}},
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("append Google Doc text: %w", err)
	}
	return &AppendResult{
		Source:        file.Source,
		DocumentID:    file.DocumentID,
		InsertedChars: utf8.RuneCountInString(text),
		Status:        "appended",
	}, nil
}

// ReplaceText replaces all occurrences of find in a folder-scoped Google Doc.
func (c *LiveClient) ReplaceText(ctx context.Context, sourceName, documentID, find, replacement string, matchCase bool) (*ReplaceResult, error) {
	src, file, err := c.getDocFile(ctx, sourceName, documentID)
	if err != nil {
		return nil, err
	}
	if find == "" {
		return nil, fmt.Errorf("find is required")
	}
	resp, err := src.docs.Documents.BatchUpdate(file.DocumentID, &docs.BatchUpdateDocumentRequest{
		Requests: []*docs.Request{{
			ReplaceAllText: &docs.ReplaceAllTextRequest{
				ContainsText: &docs.SubstringMatchCriteria{
					Text:            find,
					MatchCase:       matchCase,
					ForceSendFields: []string{"MatchCase"},
				},
				ReplaceText: replacement,
			},
		}},
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("replace Google Doc text: %w", err)
	}
	var changed int64
	for _, reply := range resp.Replies {
		if reply.ReplaceAllText != nil {
			changed += reply.ReplaceAllText.OccurrencesChanged
		}
	}
	return &ReplaceResult{
		Source:             file.Source,
		DocumentID:         file.DocumentID,
		OccurrencesChanged: changed,
		Status:             "replaced",
	}, nil
}

func (c *LiveClient) getDocFile(ctx context.Context, sourceName, documentID string) (sourceRuntime, File, error) {
	src, err := c.resolveSource(sourceName)
	if err != nil {
		return sourceRuntime{}, File{}, err
	}
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return sourceRuntime{}, File{}, fmt.Errorf("document_id is required")
	}
	if !driveIDPattern.MatchString(documentID) {
		return sourceRuntime{}, File{}, fmt.Errorf("document_id is invalid")
	}
	f, err := src.drive.Files.Get(documentID).
		Context(ctx).
		Fields(googleapi.Field("id,name,mimeType,modifiedTime,webViewLink,parents")).
		SupportsAllDrives(true).
		Do()
	if err != nil {
		return sourceRuntime{}, File{}, fmt.Errorf("get Drive file: %w", err)
	}
	if f.MimeType != GoogleDocMimeType {
		return sourceRuntime{}, File{}, fmt.Errorf("document %q is not a Google Doc", documentID)
	}
	if !containsString(f.Parents, src.source.FolderID) {
		return sourceRuntime{}, File{}, fmt.Errorf("document %q is outside configured folder for source %q", documentID, src.source.Name)
	}
	return src, fileFromDrive(src.source.Name, f), nil
}

func fileFromDrive(source string, f *drive.File) File {
	return File{
		Source:       source,
		DocumentID:   f.Id,
		Name:         f.Name,
		ModifiedTime: f.ModifiedTime,
		WebViewLink:  f.WebViewLink,
	}
}

func driveDocsQuery(folderID, query string) string {
	parts := []string{
		fmt.Sprintf("'%s' in parents", escapeDriveQueryLiteral(folderID)),
		"trashed = false",
		fmt.Sprintf("mimeType = '%s'", GoogleDocMimeType),
	}
	query = strings.TrimSpace(query)
	if query != "" {
		q := escapeDriveQueryLiteral(query)
		parts = append(parts, fmt.Sprintf("(name contains '%s' or fullText contains '%s')", q, q))
	}
	return strings.Join(parts, " and ")
}

func escapeDriveQueryLiteral(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	return min(limit, maxListLimit)
}

func truncateRunes(s string, maxChars int) string {
	if maxChars <= 0 {
		return s
	}
	count := 0
	for i := range s {
		if count == maxChars {
			return s[:i]
		}
		count++
	}
	return s
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

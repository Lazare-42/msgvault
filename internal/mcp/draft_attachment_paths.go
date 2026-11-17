package mcp

import (
	"errors"
	"fmt"
	"io"
	stdmime "mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/gmail"
)

// parseAttachmentPathArgs reads the "attachment_paths" draft argument: a list
// of absolute local file paths. Returns (nil, nil) when it is absent.
func parseAttachmentPathArgs(args map[string]any) ([]string, error) {
	raw, found := args["attachment_paths"]
	if !found || raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("attachment_paths must be an array of absolute file paths")
	}
	paths := make([]string, 0, len(items))
	for i, item := range items {
		path, _ := item.(string)
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("attachment_paths[%d]: must be a non-empty string", i)
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("attachment_paths[%d]: %q is not an absolute path", i, path)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// readDraftAttachmentPath reads one local file for a draft. The resolved
// path, after following symlinks, must be a regular file inside one of dirs,
// so a draft cannot pick up credentials or other files outside the folders
// the owner configured.
func readDraftAttachmentPath(path string, dirs []string) (gmail.DraftAttachment, error) {
	if len(dirs) == 0 {
		return gmail.DraftAttachment{}, errors.New("attaching local files is disabled; set [mail] attachment_dirs in the MCP process config")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return gmail.DraftAttachment{}, fmt.Errorf("resolve %s: %w", path, err)
	}
	inside, err := withinAttachmentDirs(real, dirs)
	if err != nil {
		return gmail.DraftAttachment{}, err
	}
	if !inside {
		return gmail.DraftAttachment{}, fmt.Errorf("%s is outside the configured attachment_dirs", path)
	}
	f, err := os.Open(real)
	if err != nil {
		return gmail.DraftAttachment{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return gmail.DraftAttachment{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return gmail.DraftAttachment{}, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxAttachmentSize {
		return gmail.DraftAttachment{}, fmt.Errorf("attachment too large: %d bytes (max %d)", info.Size(), maxAttachmentSize)
	}
	content, err := io.ReadAll(io.LimitReader(f, maxAttachmentSize+1))
	if err != nil {
		return gmail.DraftAttachment{}, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(content)) > maxAttachmentSize {
		return gmail.DraftAttachment{}, fmt.Errorf("attachment too large: more than %d bytes", maxAttachmentSize)
	}
	return gmail.DraftAttachment{
		Filename:    filepath.Base(path),
		ContentType: attachmentPathContentType(path, content),
		Content:     content,
	}, nil
}

// withinAttachmentDirs reports whether real lies under a directory matched by
// one of the configured patterns. Patterns are resolved on every call, so a
// per-session folder created after startup still matches.
func withinAttachmentDirs(real string, patterns []string) (bool, error) {
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Clean(pattern))
		if err != nil {
			return false, fmt.Errorf("invalid attachment_dirs pattern %q: %w", pattern, err)
		}
		for _, match := range matches {
			root, err := filepath.EvalSymlinks(match)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(root, real)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			return true, nil
		}
	}
	return false, nil
}

// attachmentPathContentType names the file's MIME type from its extension,
// falling back to content sniffing.
func attachmentPathContentType(path string, content []byte) string {
	if byExt := stdmime.TypeByExtension(strings.ToLower(filepath.Ext(path))); byExt != "" {
		return byExt
	}
	return http.DetectContentType(content)
}

package gmail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	stdmime "mime"
	"strings"

	gomail "github.com/emersion/go-message/mail"
)

// BuildDraftMIME builds an RFC 822 message from DraftCompose fields.
func BuildDraftMIME(d *DraftCompose) ([]byte, error) {
	if d == nil {
		return nil, errors.New("draft compose is nil")
	}

	if len(d.Attachments) == 0 {
		return buildSinglePartDraftMIME(d)
	}
	return buildMultipartDraftMIME(d)
}

func buildDraftHeader(d *DraftCompose) gomail.Header {
	var h gomail.Header
	h.Set("MIME-Version", "1.0")
	setAddressHeader(&h, "To", d.To)
	setAddressHeader(&h, "Cc", d.Cc)
	setAddressHeader(&h, "Bcc", d.Bcc)
	if d.Subject != "" {
		h.SetSubject(d.Subject)
	}
	if d.InReplyTo != "" {
		h.Set("In-Reply-To", d.InReplyTo)
	}
	references := d.References
	if references == "" {
		references = d.InReplyTo
	}
	if references != "" {
		h.Set("References", references)
	}
	return h
}

func setAddressHeader(h *gomail.Header, key string, values []string) {
	if len(values) == 0 {
		return
	}
	raw := strings.Join(values, ", ")
	addrs, err := gomail.ParseAddressList(raw)
	if err != nil {
		h.Set(key, raw)
		return
	}
	h.SetAddressList(key, addrs)
}

func draftBodyContentType(d *DraftCompose) (string, map[string]string, error) {
	contentType := d.ContentType
	if contentType == "" {
		contentType = "text/plain"
	}
	mediaType, params, err := stdmime.ParseMediaType(contentType)
	if err != nil {
		return "", nil, fmt.Errorf("parse content type %q: %w", contentType, err)
	}
	if params == nil {
		params = make(map[string]string)
	}
	if _, ok := params["charset"]; !ok {
		params["charset"] = "utf-8"
	}
	if mediaType == "text/plain" {
		if _, ok := params["format"]; !ok {
			params["format"] = "flowed"
		}
	}
	return mediaType, params, nil
}

func buildSinglePartDraftMIME(d *DraftCompose) ([]byte, error) {
	var buf bytes.Buffer
	h := buildDraftHeader(d)
	mediaType, params, err := draftBodyContentType(d)
	if err != nil {
		return nil, err
	}
	h.SetContentType(mediaType, params)
	h.Set("Content-Transfer-Encoding", "base64")

	w, err := gomail.CreateSingleInlineWriter(&buf, h)
	if err != nil {
		return nil, fmt.Errorf("create single-part draft MIME: %w", err)
	}
	if _, err := io.WriteString(w, d.Body); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("write draft body: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close draft body: %w", err)
	}
	return buf.Bytes(), nil
}

func buildMultipartDraftMIME(d *DraftCompose) ([]byte, error) {
	var buf bytes.Buffer
	h := buildDraftHeader(d)

	mw, err := gomail.CreateWriter(&buf, h)
	if err != nil {
		return nil, fmt.Errorf("create multipart draft MIME: %w", err)
	}

	mediaType, params, err := draftBodyContentType(d)
	if err != nil {
		_ = mw.Close()
		return nil, err
	}

	var bodyHeader gomail.InlineHeader
	bodyHeader.SetContentType(mediaType, params)
	bodyHeader.Set("Content-Transfer-Encoding", "base64")
	bodyWriter, err := mw.CreateSingleInline(bodyHeader)
	if err != nil {
		_ = mw.Close()
		return nil, fmt.Errorf("create draft body part: %w", err)
	}
	if _, err := io.WriteString(bodyWriter, d.Body); err != nil {
		_ = bodyWriter.Close()
		_ = mw.Close()
		return nil, fmt.Errorf("write draft body: %w", err)
	}
	if err := bodyWriter.Close(); err != nil {
		_ = mw.Close()
		return nil, fmt.Errorf("close draft body: %w", err)
	}

	for i := range d.Attachments {
		if err := writeDraftAttachment(mw, &d.Attachments[i]); err != nil {
			_ = mw.Close()
			return nil, err
		}
	}

	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart draft MIME: %w", err)
	}
	return buf.Bytes(), nil
}

func writeDraftAttachment(mw *gomail.Writer, a *DraftAttachment) error {
	contentType := a.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	mediaType, params, err := stdmime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("parse attachment content type %q: %w", contentType, err)
	}
	if params == nil {
		params = make(map[string]string)
	}
	if a.Filename != "" {
		params["name"] = a.Filename
	}

	var h gomail.AttachmentHeader
	h.SetContentType(mediaType, params)
	if a.Filename != "" {
		h.SetFilename(a.Filename)
	}

	w, err := mw.CreateAttachment(h)
	if err != nil {
		return fmt.Errorf("create attachment %q: %w", a.Filename, err)
	}
	if _, err := w.Write(a.Content); err != nil {
		_ = w.Close()
		return fmt.Errorf("write attachment %q: %w", a.Filename, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close attachment %q: %w", a.Filename, err)
	}
	return nil
}

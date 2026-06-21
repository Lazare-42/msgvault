package gmail

import (
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestBuildRFC822Message_NoAttachments_SinglePart(t *testing.T) {
	d := &DraftCompose{
		To:      []string{"a@example.com"},
		Subject: "hi",
		Body:    "hello world",
	}
	raw := string(buildRFC822Message(d))
	if strings.Contains(raw, "multipart/mixed") {
		t.Fatalf("expected single-part message, got multipart:\n%s", raw)
	}
	if !strings.Contains(raw, "Content-Transfer-Encoding: base64") {
		t.Fatalf("expected base64 body encoding:\n%s", raw)
	}
}

func TestBuildRFC822Message_ReplyHeaders(t *testing.T) {
	t.Run("in_reply_to defaults references", func(t *testing.T) {
		d := &DraftCompose{
			To:        []string{"a@example.com"},
			Subject:   "RE: hi",
			Body:      "hello",
			InReplyTo: "<orig-123@mail.gmail.com>",
		}
		msg, err := mail.ReadMessage(strings.NewReader(string(buildRFC822Message(d))))
		if err != nil {
			t.Fatalf("parse message: %v", err)
		}
		if got := msg.Header.Get("In-Reply-To"); got != "<orig-123@mail.gmail.com>" {
			t.Fatalf("In-Reply-To = %q", got)
		}
		// References defaults to In-Reply-To when not supplied.
		if got := msg.Header.Get("References"); got != "<orig-123@mail.gmail.com>" {
			t.Fatalf("References = %q (expected to default to In-Reply-To)", got)
		}
	})

	t.Run("explicit references chain", func(t *testing.T) {
		d := &DraftCompose{
			To:         []string{"a@example.com"},
			Subject:    "RE: hi",
			Body:       "hello",
			InReplyTo:  "<msg-2@x>",
			References: "<msg-0@x> <msg-1@x> <msg-2@x>",
		}
		msg, err := mail.ReadMessage(strings.NewReader(string(buildRFC822Message(d))))
		if err != nil {
			t.Fatalf("parse message: %v", err)
		}
		if got := msg.Header.Get("References"); got != "<msg-0@x> <msg-1@x> <msg-2@x>" {
			t.Fatalf("References = %q", got)
		}
	})

	t.Run("no reply headers when not a reply", func(t *testing.T) {
		d := &DraftCompose{To: []string{"a@example.com"}, Subject: "hi", Body: "hello"}
		raw := string(buildRFC822Message(d))
		if strings.Contains(raw, "In-Reply-To:") || strings.Contains(raw, "References:") {
			t.Fatalf("unexpected reply headers in non-reply draft:\n%s", raw)
		}
	})
}

func TestBuildRFC822Message_WithAttachments_Multipart(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake cession bytes \x00\x01\x02")
	d := &DraftCompose{
		To:      []string{"pdrion@phildev.dev", "philippedrion@gmail.com"},
		Subject: "Cession — pièces jointes",
		Body:    "Voici les pièces.",
		Attachments: []DraftAttachment{
			{Filename: "Cession ROSSILLON-PHILDEV.pdf", ContentType: "application/pdf", Content: pdf},
		},
	}

	raw := buildRFC822Message(d)
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content-type: %v", err)
	}
	if mediaType != "multipart/mixed" {
		t.Fatalf("expected multipart/mixed, got %q", mediaType)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])

	// Part 1: body.
	body, err := mr.NextPart()
	if err != nil {
		t.Fatalf("read body part: %v", err)
	}
	if ct := body.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("expected text/plain body part, got %q", ct)
	}

	// Part 2: attachment — verify content round-trips through base64.
	att, err := mr.NextPart()
	if err != nil {
		t.Fatalf("read attachment part: %v", err)
	}
	if disp := att.Header.Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment") {
		t.Fatalf("expected attachment disposition, got %q", disp)
	}
	encoded, err := io.ReadAll(att)
	if err != nil {
		t.Fatalf("read attachment body: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(encoded), "\r\n", ""))
	if err != nil {
		t.Fatalf("decode attachment base64: %v", err)
	}
	if string(decoded) != string(pdf) {
		t.Fatalf("attachment content mismatch: got %q want %q", decoded, pdf)
	}

	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("expected exactly 2 parts, got extra/err: %v", err)
	}
}

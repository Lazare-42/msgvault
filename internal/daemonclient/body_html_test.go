package daemonclient

import (
	"testing"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedDetailKeepsBodyHTML(t *testing.T) {
	html := "<p>items</p>"

	// Stub text part + HTML: both survive, text stays text.
	msg := generatedDetailToAPIMessage(&generated.MessageDetail{Body: "stub", BodyHTML: &html})
	if msg.BodyText != "stub" || msg.BodyHTML != html {
		t.Fatalf("text+html: got text=%q html=%q", msg.BodyText, msg.BodyHTML)
	}
	detail := queryDetailFromAPIMessage(msg)
	if detail.BodyText != "stub" || detail.BodyHTML != html {
		t.Fatalf("detail text+html: got text=%q html=%q", detail.BodyText, detail.BodyHTML)
	}

	// HTML-only: the daemon's body is the HTML fallback; it must not be
	// reported as text.
	msg = generatedDetailToAPIMessage(&generated.MessageDetail{Body: html, BodyHTML: &html})
	detail = queryDetailFromAPIMessage(msg)
	if detail.BodyText != "" || detail.BodyHTML != html {
		t.Fatalf("html-only: got text=%q html=%q", detail.BodyText, detail.BodyHTML)
	}

	// Text-only: unchanged.
	detail = queryDetailFromAPIMessage(&store.APIMessage{Body: "plain"})
	if detail.BodyText != "plain" || detail.BodyHTML != "" {
		t.Fatalf("text-only: got text=%q html=%q", detail.BodyText, detail.BodyHTML)
	}
}

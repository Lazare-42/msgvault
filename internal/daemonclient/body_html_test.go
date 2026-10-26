package daemonclient

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedDetailKeepsBodyHTML(t *testing.T) {
	assert := assert.New(t)
	html := "<p>items</p>"

	// Stub text part + HTML: both survive, text stays text.
	msg := generatedDetailToAPIMessage(&generated.MessageDetail{Body: "stub", BodyHTML: &html})
	assert.Equal("stub", msg.BodyText)
	assert.Equal(html, msg.BodyHTML)
	detail := queryDetailFromAPIMessage(msg)
	assert.Equal("stub", detail.BodyText)
	assert.Equal(html, detail.BodyHTML)

	// HTML-only: the daemon's body is the HTML fallback; it must not be
	// reported as text.
	msg = generatedDetailToAPIMessage(&generated.MessageDetail{Body: html, BodyHTML: &html})
	detail = queryDetailFromAPIMessage(msg)
	assert.Equal("", detail.BodyText)
	assert.Equal(html, detail.BodyHTML)

	// Text-only: unchanged.
	detail = queryDetailFromAPIMessage(&store.APIMessage{Body: "plain"})
	assert.Equal("plain", detail.BodyText)
	assert.Equal("", detail.BodyHTML)
}

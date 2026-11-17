package daemonclient

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/msgvault/pkg/client/generated"
)

// The daemon sends body_text separately from body, so an HTML-only message
// keeps its HTML and is never reported as plain text.
func TestGeneratedDetailKeepsBodyHTML(t *testing.T) {
	assert := assert.New(t)
	html := "<p>items</p>"
	stub, empty := "stub", ""

	// Text part + HTML: both survive, text stays text.
	msg := generatedDetailToAPIMessage(&generated.MessageDetail{Body: stub, BodyText: &stub, BodyHTML: &html})
	assert.Equal("stub", msg.BodyText)
	assert.Equal(html, msg.BodyHTML)
	detail := queryDetailFromAPIMessage(msg)
	assert.Equal("stub", detail.BodyText)
	assert.Equal(html, detail.BodyHTML)

	// HTML-only: body carries the HTML fallback, body_text is empty.
	msg = generatedDetailToAPIMessage(&generated.MessageDetail{Body: html, BodyText: &empty, BodyHTML: &html})
	detail = queryDetailFromAPIMessage(msg)
	assert.Empty(detail.BodyText)
	assert.Equal(html, detail.BodyHTML)
}

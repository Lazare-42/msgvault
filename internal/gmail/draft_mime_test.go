package gmail

import (
	"bytes"
	"net/mail"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDraftMIMEGeneratesDistinctMessageIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	compose := &DraftCompose{To: []string{"reader@example.test"}, Body: "Reply", InReplyTo: "<parent@example.test>"}
	ids := make([]string, 2)
	for i := range ids {
		raw, err := BuildDraftMIME(compose)
		require.NoError(err)
		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		require.NoError(err)
		ids[i] = parsed.Header.Get("Message-ID")
		assert.Regexp(`^<[0-9a-f-]+@msgvault\.local>$`, ids[i])
		assert.Equal("<parent@example.test>", parsed.Header.Get("In-Reply-To"))
		assert.Equal("<parent@example.test>", parsed.Header.Get("References"))
	}
	assert.NotEqual(ids[0], ids[1])
}

func TestParseDraftReadsThreadingHeaders(t *testing.T) {
	draft := parseDraft(gmailDraft{ID: "d1", Message: gmailDraftMessage{Payload: &gmailPayload{Headers: []gmailHeader{
		{Name: "In-Reply-To", Value: "<parent@example.com>"},
		{Name: "References", Value: "<root@example.com> <parent@example.com>"},
	}}}})
	assert.Equal(t, "<parent@example.com>", draft.Message.InReplyTo)
	assert.Equal(t, "<root@example.com> <parent@example.com>", draft.Message.References)
}

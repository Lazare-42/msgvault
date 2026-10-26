package cmd

import (
	"bytes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWhatsAppPairingAuth(t *testing.T) {
	handler := &whatsappPairingHandler{pairingToken: "secret"}

	t.Run("query_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/qr?token=secret", nil)
		assert.True(t, handler.authorized(req))
	})

	t.Run("bearer_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/qr", nil)
		req.Header.Set("Authorization", "Bearer secret")
		assert.True(t, handler.authorized(req))
	})

	t.Run("wrong_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/qr?token=wrong", nil)
		assert.False(t, handler.authorized(req))
	})

	t.Run("open_when_unconfigured", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/qr", nil)
		assert.True(t, (&whatsappPairingHandler{}).authorized(req))
	})
}

func TestNormalizeHTTPBasePath(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := normalizeHTTPBasePath("")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("root", func(t *testing.T) {
		got, err := normalizeHTTPBasePath("/")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("trims_trailing_slash", func(t *testing.T) {
		got, err := normalizeHTTPBasePath("/personal/")
		require.NoError(t, err)
		assert.Equal(t, "/personal", got)
	})

	t.Run("requires_absolute_path", func(t *testing.T) {
		_, err := normalizeHTTPBasePath("personal")
		require.Error(t, err)
		assert.ErrorContains(t, err, "must start with /")
	})

	t.Run("rejects_query", func(t *testing.T) {
		_, err := normalizeHTTPBasePath("/personal?token=x")
		require.Error(t, err)
		assert.ErrorContains(t, err, "query or fragment")
	})
}

func TestWhatsAppPairingPublicPath(t *testing.T) {
	assert.Equal(t, "/qr", (&whatsappPairingHandler{}).publicPath("/qr"))
	assert.Equal(t, "/work/qr", (&whatsappPairingHandler{basePath: "/work"}).publicPath("/qr"))
}

func TestNormalizeHTTPPublicBaseURL(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := normalizeHTTPPublicBaseURL("")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("trims_trailing_slash", func(t *testing.T) {
		got, err := normalizeHTTPPublicBaseURL("https://whats.lazare.ai/work/")
		require.NoError(t, err)
		assert.Equal(t, "https://whats.lazare.ai/work", got)
	})

	t.Run("requires_http_url", func(t *testing.T) {
		_, err := normalizeHTTPPublicBaseURL("whats.lazare.ai/work")
		require.Error(t, err)
		assert.ErrorContains(t, err, "http:// or https://")
	})

	t.Run("rejects_query", func(t *testing.T) {
		_, err := normalizeHTTPPublicBaseURL("https://whats.lazare.ai/work?token=x")
		require.Error(t, err)
		assert.ErrorContains(t, err, "query or fragment")
	})
}

func TestWhatsAppLoginPageURL(t *testing.T) {
	assert.Equal(t, "https://whats.lazare.ai/work/qr", whatsappLoginPageURL("https://whats.lazare.ai/work", "/ignored"))
	assert.Equal(t, "/work/qr", whatsappLoginPageURL("", "/work"))
	assert.Equal(t, "/qr", whatsappLoginPageURL("", ""))
}

func TestWhatsAppPairingTemplateShowsIncompletePairing(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	err := whatsappPairingTemplate.Execute(&buf, whatsappPairingView{
		Authorized:          true,
		NeedsAuthentication: true,
		Status: whatsapplive.Status{
			AccountJID:  "15551234567@s.whatsapp.net",
			Connected:   true,
			LoggedIn:    false,
			Paired:      true,
			SessionPath: "/tmp/whatsapp-session.db",
		},
	})
	require.NoError(t, err)

	html := buf.String()
	assert.Contains(html, "Pairing incomplete.")
	assert.Contains(html, "Logged in: <code>false</code>")
	assert.NotContains(html, "Linked.")
}

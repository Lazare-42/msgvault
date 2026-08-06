package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

func TestWhatsAppPairingAuth(t *testing.T) {
	handler := &whatsappPairingHandler{pairingToken: "secret"}

	t.Run("query_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/qr?token=secret", nil)
		assertpkg.True(t, handler.authorized(req))
	})

	t.Run("bearer_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/qr", nil)
		req.Header.Set("Authorization", "Bearer secret")
		assertpkg.True(t, handler.authorized(req))
	})

	t.Run("wrong_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/qr?token=wrong", nil)
		assertpkg.False(t, handler.authorized(req))
	})

	t.Run("open_when_unconfigured", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/qr", nil)
		assertpkg.True(t, (&whatsappPairingHandler{}).authorized(req))
	})
}

func TestNormalizeHTTPBasePath(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := normalizeHTTPBasePath("")
		requirepkg.NoError(t, err)
		assertpkg.Empty(t, got)
	})

	t.Run("root", func(t *testing.T) {
		got, err := normalizeHTTPBasePath("/")
		requirepkg.NoError(t, err)
		assertpkg.Empty(t, got)
	})

	t.Run("trims_trailing_slash", func(t *testing.T) {
		got, err := normalizeHTTPBasePath("/personal/")
		requirepkg.NoError(t, err)
		assertpkg.Equal(t, "/personal", got)
	})

	t.Run("requires_absolute_path", func(t *testing.T) {
		_, err := normalizeHTTPBasePath("personal")
		requirepkg.Error(t, err)
		assertpkg.ErrorContains(t, err, "must start with /")
	})

	t.Run("rejects_query", func(t *testing.T) {
		_, err := normalizeHTTPBasePath("/personal?token=x")
		requirepkg.Error(t, err)
		assertpkg.ErrorContains(t, err, "query or fragment")
	})
}

func TestWhatsAppPairingPublicPath(t *testing.T) {
	assertpkg.Equal(t, "/qr", (&whatsappPairingHandler{}).publicPath("/qr"))
	assertpkg.Equal(t, "/work/qr", (&whatsappPairingHandler{basePath: "/work"}).publicPath("/qr"))
}

func TestNormalizeHTTPPublicBaseURL(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := normalizeHTTPPublicBaseURL("")
		requirepkg.NoError(t, err)
		assertpkg.Empty(t, got)
	})

	t.Run("trims_trailing_slash", func(t *testing.T) {
		got, err := normalizeHTTPPublicBaseURL("https://whats.lazare.ai/work/")
		requirepkg.NoError(t, err)
		assertpkg.Equal(t, "https://whats.lazare.ai/work", got)
	})

	t.Run("requires_http_url", func(t *testing.T) {
		_, err := normalizeHTTPPublicBaseURL("whats.lazare.ai/work")
		requirepkg.Error(t, err)
		assertpkg.ErrorContains(t, err, "http:// or https://")
	})

	t.Run("rejects_query", func(t *testing.T) {
		_, err := normalizeHTTPPublicBaseURL("https://whats.lazare.ai/work?token=x")
		requirepkg.Error(t, err)
		assertpkg.ErrorContains(t, err, "query or fragment")
	})
}

func TestWhatsAppLoginPageURL(t *testing.T) {
	assertpkg.Equal(t, "https://whats.lazare.ai/work/qr", whatsappLoginPageURL("https://whats.lazare.ai/work", "/ignored"))
	assertpkg.Equal(t, "/work/qr", whatsappLoginPageURL("", "/work"))
	assertpkg.Equal(t, "/qr", whatsappLoginPageURL("", ""))
}

func TestWhatsAppPairingTemplateShowsIncompletePairing(t *testing.T) {
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
	requirepkg.NoError(t, err)

	html := buf.String()
	assertpkg.Contains(t, html, "Pairing incomplete.")
	assertpkg.Contains(t, html, "Logged in: <code>false</code>")
	assertpkg.NotContains(t, html, "Linked.")
}

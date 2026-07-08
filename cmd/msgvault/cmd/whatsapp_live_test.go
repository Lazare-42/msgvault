package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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

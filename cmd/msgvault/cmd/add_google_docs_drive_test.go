package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestAddGoogleDocsDriveWritesConfigWithoutSecrets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	home := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	ctx := withStoreResolverConfig(t, cfg)
	cmd := newTestRootCmd()
	cmd.SetContext(ctx)
	cmd.AddCommand(newAddGoogleDocsDriveCmd())
	cmd.SetArgs([]string{
		"add-google-docs-drive", "docs",
		"--folder-id", "drive-folder-id",
		"--google-account", "user@example.com",
		"--oauth-app", "personal",
		"--skip-auth-for-test",
	})
	require.NoError(cmd.Execute(), "Execute")

	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(err, "read config")
	text := string(data)
	for _, want := range []string{
		`[[google_docs.sources]]`,
		`name = "docs"`,
		`enabled = true`,
		`folder_id = "drive-folder-id"`,
		`google_account = "user@example.com"`,
		`oauth_app = "personal"`,
	} {
		require.Contains(text, want, "config missing %q", want)
	}
	lower := strings.ToLower(text)
	refreshTokenKey := "refresh" + "_token"
	clientSecretKey := "client" + "_secret\""
	assert.NotContains(lower, refreshTokenKey, "config contains secret material:\n%s", text)
	assert.NotContains(lower, clientSecretKey, "config contains secret material:\n%s", text)
}

func TestGoogleDocsDriveTokenReadyRequiresDriveAndDocsScopes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	home := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	ctx := withStoreResolverConfig(t, cfg)
	secretsPath := filepath.Join(home, "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0o600), "write client secrets")
	mgr, err := newGoogleDocsDriveOAuthManager(ctx, config.OAuthApp{ClientSecrets: secretsPath})
	require.NoError(err, "newGoogleDocsDriveOAuthManager")

	const email = "user@example.com"
	assert.False(googleDocsDriveTokenReady(ctx, mgr, email), "missing token")

	writeGoogleDocsTokenFile(t, home, email, []string{"https://www.googleapis.com/auth/gmail.readonly"})
	assert.False(googleDocsDriveTokenReady(ctx, mgr, email), "gmail-only token should not satisfy Drive/Docs")

	writeGoogleDocsTokenFile(t, home, email, googleDocsDriveScopes())
	assert.True(googleDocsDriveTokenReady(ctx, mgr, email), "Drive/Docs token should be ready")
}

func writeGoogleDocsTokenFile(t *testing.T, home, email string, scopes []string) {
	t.Helper()
	tokensDir := filepath.Join(home, "tokens")
	require.NoError(t, os.MkdirAll(tokensDir, 0o700), "mkdir tokens")
	data, err := json.Marshal(map[string]any{
		"access_token": "test",
		"token_type":   "Bearer",
		"expiry":       time.Now().Add(time.Hour).Format(time.RFC3339),
		"scopes":       scopes,
	})
	require.NoError(t, err, "marshal token")
	require.NoError(t, os.WriteFile(filepath.Join(tokensDir, email+".json"), data, 0o600), "write token")
}

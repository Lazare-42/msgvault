package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestAddGoogleDocsDriveWritesConfigWithoutSecrets(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })

	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cmd := newTestRootCmd()
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })

	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	secretsPath := filepath.Join(home, "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0o600), "write client secrets")
	mgr, err := newGoogleDocsDriveOAuthManager(secretsPath)
	require.NoError(err, "newGoogleDocsDriveOAuthManager")

	const email = "user@example.com"
	assert.False(googleDocsDriveTokenReady(mgr, email), "missing token")

	writeGoogleDocsTokenFile(t, home, email, []string{"https://www.googleapis.com/auth/gmail.readonly"})
	assert.False(googleDocsDriveTokenReady(mgr, email), "gmail-only token should not satisfy Drive/Docs")

	writeGoogleDocsTokenFile(t, home, email, googleDocsDriveScopes())
	assert.True(googleDocsDriveTokenReady(mgr, email), "Drive/Docs token should be ready")
}

func writeGoogleDocsTokenFile(t *testing.T, home, email string, scopes []string) {
	t.Helper()
	tokensDir := filepath.Join(home, "tokens")
	requirepkg.NoError(t, os.MkdirAll(tokensDir, 0o700), "mkdir tokens")
	data, err := json.Marshal(map[string]any{
		"access_token": "test",
		"token_type":   "Bearer",
		"expiry":       time.Now().Add(time.Hour).Format(time.RFC3339),
		"scopes":       scopes,
	})
	requirepkg.NoError(t, err, "marshal token")
	requirepkg.NoError(t, os.WriteFile(filepath.Join(tokensDir, email+".json"), data, 0o600), "write token")
}

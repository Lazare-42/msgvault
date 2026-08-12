package cmd

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
)

// startMemIMAPServer runs an in-memory IMAP server for factory tests and
// returns its listen port.
func startMemIMAPServer(t *testing.T, username, password string) int {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(username, password)
	require.NoError(t, user.Create("INBOX", nil))
	memServer.AddUser(user)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps: goimap.CapSet{
			goimap.CapIMAP4rev1: {},
			goimap.CapUIDPlus:   {},
		},
		InsecureAuth: true,
		Logger:       log.New(io.Discard, "", 0),
	})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	return listener.Addr().(*net.TCPAddr).Port
}

// TestBuildIMAPAPIClient exercises the shared IMAP client constructor used by
// both the sync path and the MCP write-tool factory: password credentials are
// loaded from the tokens dir and the resulting client talks to a real IMAP
// server.
func TestBuildIMAPAPIClient(t *testing.T) {
	const (
		identifier = "user@example.com"
		password   = "imap-test-password"
	)

	tmpDir := t.TempDir()
	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	port := startMemIMAPServer(t, identifier, password)
	require.NoError(t, imaplib.SaveCredentials(cfg.TokensDir(), identifier, password))

	imapCfg := &imaplib.Config{Host: "127.0.0.1", Port: port, Username: identifier}
	syncConfig, err := imapCfg.ToJSON()
	require.NoError(t, err)

	t.Run("password auth connects and lists labels", func(t *testing.T) {
		client, err := buildIMAPAPIClient(context.Background(), identifier, syncConfig)
		require.NoError(t, err)
		defer func() { assert.NoError(t, client.Close()) }()

		labels, err := client.ListLabels(context.Background())
		require.NoError(t, err)
		names := make([]string, 0, len(labels))
		for _, l := range labels {
			names = append(names, l.Name)
		}
		assert.Contains(t, names, "INBOX")
	})

	t.Run("empty sync config errors", func(t *testing.T) {
		_, err := buildIMAPAPIClient(context.Background(), identifier, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no config")
	})

	t.Run("malformed sync config errors", func(t *testing.T) {
		_, err := buildIMAPAPIClient(context.Background(), identifier, "{not json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse IMAP config")
	})

	t.Run("missing credentials errors", func(t *testing.T) {
		otherCfg := &imaplib.Config{Host: "127.0.0.1", Port: port, Username: "nobody@example.com"}
		otherJSON, err := otherCfg.ToJSON()
		require.NoError(t, err)
		_, err = buildIMAPAPIClient(context.Background(), "nobody@example.com", otherJSON)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "load IMAP credentials")
	})

	t.Run("app-only xoauth2 dispatches to certificate flow", func(t *testing.T) {
		appOnlyCfg := &imaplib.Config{
			Host:       "127.0.0.1",
			Port:       port,
			Username:   identifier,
			AuthMethod: imaplib.AuthXOAuth2,
			MSAppOnly:  true,
			MSTenantID: "test-tenant",
			MSClientID: "test-client",
			MSCertPath: tmpDir + "/does-not-exist.pem",
		}
		appOnlyJSON, err := appOnlyCfg.ToJSON()
		require.NoError(t, err)
		// The certificate file does not exist, so the app-only token
		// source constructor must fail — proving the XOAUTH2/app-only
		// branch is reached without any Google OAuth configuration.
		_, err = buildIMAPAPIClient(context.Background(), identifier, appOnlyJSON)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "app-only token source")
	})
}

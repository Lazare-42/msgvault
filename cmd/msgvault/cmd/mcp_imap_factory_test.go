package cmd

import (
	"context"
	"encoding/json"
	"go.kenn.io/msgvault/internal/daemonclient"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
)

// startMemIMAPServer runs an in-memory IMAP server for factory tests and
// returns its listen port.
func startMemIMAPServer(t *testing.T, username, password string) int {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(username, password)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Drafts", nil))
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

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	return addr.Port
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
	cfg := &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	ctx := withStoreResolverConfig(t, cfg)

	port := startMemIMAPServer(t, identifier, password)
	require.NoError(t, imaplib.SaveCredentials(cfg.TokensDir(), identifier, password))

	imapCfg := &imaplib.Config{Host: "127.0.0.1", Port: port, Username: identifier}
	syncConfig, err := imapCfg.ToJSON()
	require.NoError(t, err)

	t.Run("password auth connects and lists labels", func(t *testing.T) {
		client, err := buildIMAPAPIClient(ctx, identifier, syncConfig)
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
		_, err := buildIMAPAPIClient(ctx, identifier, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no config")
	})

	t.Run("malformed sync config errors", func(t *testing.T) {
		_, err := buildIMAPAPIClient(ctx, identifier, "{not json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse IMAP config")
	})

	t.Run("missing credentials errors", func(t *testing.T) {
		otherCfg := &imaplib.Config{Host: "127.0.0.1", Port: port, Username: "nobody@example.com"}
		otherJSON, err := otherCfg.ToJSON()
		require.NoError(t, err)
		_, err = buildIMAPAPIClient(ctx, "nobody@example.com", otherJSON)
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
		_, err = buildIMAPAPIClient(ctx, identifier, appOnlyJSON)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "app-only token source")
	})
}

func TestLiveMailFactoryRetainsInvocationForHTTPRequest(t *testing.T) {
	require := require.New(t)
	const email, password = "user@example.com", "test-password"
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	ctx := withStoreResolverConfig(t, cfg)
	port := startMemIMAPServer(t, email, password)
	require.NoError(imaplib.SaveCredentials(cfg.TokensDir(), email, password))
	imapCfg := &imaplib.Config{Host: "127.0.0.1", Port: port, Username: email}
	syncConfig, err := imapCfg.ToJSON()
	require.NoError(err)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/cli/accounts", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]any{{
			"email": email, "type": "imap", "sync_config": syncConfig,
		}}}))
	}))
	defer daemon.Close()
	st, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true})
	require.NoError(err)
	defer func() { assert.NoError(t, st.Close()) }()
	factory := buildMailFactory(ctx, st)
	require.NotNil(factory)
	// HTTP request contexts do not carry the CLI invocation.
	client, err := factory(t.Context(), email)
	require.NoError(err)
	defer func() { assert.NoError(t, client.Close()) }()
	labels, err := client.ListLabels(t.Context())
	require.NoError(err)
	require.NotEmpty(labels)
}

// IMAP sources are stored under their imaps:// identifier. Drafts must still
// default to, and allowlist, the login address rather than the URL.
func TestLiveMailFactoryUsesIMAPLoginAsSender(t *testing.T) {
	require := require.New(t)
	const email, password = "owner@example.com", "test-password"
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Mail.Accounts = map[string]config.MailAccountConfig{
		email: {AllowedFrom: []string{"reports@example.com"}},
	}
	ctx := withStoreResolverConfig(t, cfg)
	port := startMemIMAPServer(t, email, password)
	imapCfg := &imaplib.Config{Host: "127.0.0.1", Port: port, Username: email}
	identifier := imapCfg.Identifier()
	require.NoError(imaplib.SaveCredentials(cfg.TokensDir(), identifier, password))
	syncConfig, err := imapCfg.ToJSON()
	require.NoError(err)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]any{{
			"email": identifier, "type": "imap", "sync_config": syncConfig,
		}}}))
	}))
	defer daemon.Close()
	st, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true})
	require.NoError(err)
	defer func() { assert.NoError(t, st.Close()) }()
	factory := buildMailFactory(ctx, st)
	require.NotNil(factory)

	client, err := factory(t.Context(), identifier)
	require.NoError(err)
	defer func() { assert.NoError(t, client.Close()) }()

	draft, err := client.CreateDraft(t.Context(), &gmail.DraftCompose{
		To: []string{"reader@example.com"}, Subject: "default sender", Body: "body",
	})
	require.NoError(err)
	read, err := client.GetDraft(t.Context(), draft.ID)
	require.NoError(err)
	assert.Contains(t, read.Message.From, email)

	_, err = client.CreateDraft(t.Context(), &gmail.DraftCompose{
		From: "Reports <reports@example.com>", To: []string{"reader@example.com"}, Subject: "alternate", Body: "body",
	})
	assert.NoError(t, err, "allowed_from keyed by the login address applies to the IMAP source")
}

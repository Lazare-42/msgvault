package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/googledocs"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/oauth"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

var mcpForceSQL bool
var mcpNoSQLiteScanner bool
var mcpHTTPAddr string
var mcpHTTPAllowInsecure bool
var serveMCPHTTPWithOptions = mcpserver.ServeHTTPWithOptions

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run MCP server for Claude Desktop integration",
	Long: `Start an MCP (Model Context Protocol) server over stdio.

This allows Claude Desktop (or any MCP client) to query your email archive
using tools like search_metadata, search_message_bodies, semantic_search_messages,
get_message, list_messages, get_stats, aggregate, stage_deletion, and draft
management (list, create, update, send, delete drafts).

Add to Claude Desktop config:
  {
    "mcpServers": {
      "msgvault": {
        "command": "msgvault",
        "args": ["mcp"]
      }
	    }
	  }`,
	RunE: func(cmd *cobra.Command, args []string) error {
		st, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return fmt.Errorf("open daemon: %w", err)
		}
		defer func() { _ = st.Close() }()

		// Derive from cmd.Context() so signal handling installed by
		// the cobra root command (SIGINT/SIGTERM → ctx.Done()) reaches
		// the MCP transport and can trigger ServeHTTPWithOptions's
		// graceful shutdown.
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		opts, err := daemonMCPServeOptions(ctx, st)
		if err != nil {
			return err
		}
		// Set up the live mail client factory for draft/label operations.
		// If neither Gmail OAuth nor an IMAP account is available, the
		// write tools are simply not exposed.
		opts.GmailFactory = buildGmailFactory(ctx, st)
		opts.GoogleDocsFactory = buildGoogleDocsFactory()

		if mcpHTTPAddr != "" {
			normalized, err := normalizeMCPHTTPAddr(
				mcpHTTPAddr,
				mcpHTTPAllowInsecure,
				cfg.Server.APIKey != "",
			)
			if err != nil {
				return usageErr(cmd, err)
			}
			return serveMCPHTTPWithOptions(ctx, opts, normalized, cfg.Server.APIKey)
		}
		return mcpserver.ServeWithOptions(ctx, opts)
	},
}

func daemonMCPServeOptions(ctx context.Context, st *daemonclient.Client) (mcpserver.ServeOptions, error) {
	opts := mcpserver.ServeOptions{
		Engine:           daemonclient.NewEngineAdapter(st),
		AttachmentsDir:   cfg.AttachmentsDir(),
		AttachmentReader: st,
		ManifestSaver:    daemonMCPManifestSaver{client: st},
		OCR:              st,
		DataDir:          cfg.Data.DataDir,
	}

	vectorAvailable, err := st.VectorSearchAvailable(ctx)
	if err != nil {
		return mcpserver.ServeOptions{}, fmt.Errorf("check daemon vector search: %w", err)
	}
	if vectorAvailable {
		opts.HybridSearcher = daemonMCPHybridSearcher{client: st}
		opts.SimilarSearcher = daemonMCPSimilarSearcher{client: st}
	}
	return opts, nil
}

type daemonMCPHybridSearcher struct {
	client *daemonclient.Client
}

type daemonMCPManifestSaver struct {
	client *daemonclient.Client
}

func (s daemonMCPManifestSaver) SaveManifest(ctx context.Context, manifest *deletion.Manifest) error {
	_, err := s.client.CreateCLIDeletionManifest(ctx, manifest)
	return err
}

func (s daemonMCPHybridSearcher) SearchHybrid(
	ctx context.Context,
	req mcpserver.HybridSearchRequest,
) (*mcpserver.HybridSearchResult, error) {
	resp, err := s.client.GetCLIHybridSearch(ctx, daemonclient.CLIHybridSearchRequest{
		Query:          req.Query,
		Account:        req.Account,
		Mode:           req.Mode,
		Limit:          req.Limit,
		Offset:         req.Offset,
		IncludeMatches: req.IncludeMatches,
		MinScore:       req.MinScore,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &mcpserver.HybridSearchResult{}, nil
	}

	hits := make([]mcpserver.HybridSearchHit, len(resp.Results))
	for i, hit := range resp.Results {
		out := mcpserver.HybridSearchHit{
			ID:               hit.ID,
			RRFScore:         hit.RRFScore,
			BM25Score:        hit.BM25Score,
			VectorScore:      hit.VectorScore,
			SubjectBoosted:   hit.SubjectBoosted,
			MatchesTruncated: hit.MatchesTruncated,
		}
		if len(hit.Matches) > 0 {
			out.Matches = make([]mcpserver.HybridSearchMatch, len(hit.Matches))
			for j, match := range hit.Matches {
				out.Matches[j] = mcpserver.HybridSearchMatch{
					CharOffset: match.CharOffset,
					Snippet:    match.Snippet,
					Line:       match.Line,
					Score:      match.Score,
				}
			}
		}
		hits[i] = out
	}
	return &mcpserver.HybridSearchResult{
		Hits:          hits,
		PoolSaturated: resp.PoolSaturated,
		HasMore:       resp.HasMore,
		Generation: mcpserver.HybridGeneration{
			ID:          resp.Generation.ID,
			Model:       resp.Generation.Model,
			Dimension:   resp.Generation.Dimension,
			Fingerprint: resp.Generation.Fingerprint,
			State:       resp.Generation.State,
		},
	}, nil
}

type daemonMCPSimilarSearcher struct {
	client *daemonclient.Client
}

func (s daemonMCPSimilarSearcher) FindSimilar(
	ctx context.Context,
	req mcpserver.SimilarSearchRequest,
) (*mcpserver.SimilarSearchResult, error) {
	resp, err := s.client.FindSimilarMessages(ctx, daemonclient.SimilarSearchRequest{
		MessageID:     req.MessageID,
		Limit:         req.Limit,
		Account:       req.Account,
		MessageType:   req.MessageType,
		After:         req.After,
		Before:        req.Before,
		HasAttachment: req.HasAttachment,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &mcpserver.SimilarSearchResult{SeedMessageID: req.MessageID}, nil
	}
	return &mcpserver.SimilarSearchResult{
		SeedMessageID: resp.SeedMessageID,
		Generation: mcpserver.HybridGeneration{
			ID:          resp.Generation.ID,
			Model:       resp.Generation.Model,
			Dimension:   resp.Generation.Dimension,
			Fingerprint: resp.Generation.Fingerprint,
			State:       resp.Generation.State,
		},
		Messages: resp.Messages,
	}, nil
}

// buildGmailFactory returns a GmailClientFactory that creates authenticated
// live mail clients for the MCP draft/label write tools. Gmail accounts use
// Google OAuth tokens; IMAP accounts (including Microsoft 365 delegated and
// app-only sources) build an IMAP/SMTP client from the source's sync_config.
// Returns nil — disabling the write tools — only when Google OAuth is not
// configured AND no IMAP account exists in the archive.
//
// The stdio/HTTP mcp command talks to the daemon over a *daemonclient.Client
// rather than a direct *store.Store, so account/OAuth-app/sync_config lookups
// are resolved through the daemon's CLI accounts API. Credentials themselves
// (OAuth tokens, IMAP passwords, Microsoft refresh tokens or certificates)
// live locally (cfg.TokensDir() or configured paths) and are read directly.
func buildGmailFactory(ctx context.Context, st *daemonclient.Client) mcpserver.GmailClientFactory {
	secretsPath, secretsErr := cfg.OAuth.ClientSecretsFor("")
	oauthConfigured := secretsErr == nil

	hasIMAP := false
	if accounts, err := st.GetCLIAccounts(ctx); err == nil {
		for _, a := range accounts {
			if a.Type == sourceTypeIMAP {
				hasIMAP = true
				break
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "Note: could not list accounts to detect IMAP sources: %v\n", err)
	}

	if !oauthConfigured && !hasIMAP {
		// Neither auth path is available — write tools will be disabled.
		fmt.Fprintf(os.Stderr, "Note: OAuth not configured and no IMAP accounts, draft tools disabled\n")
		return nil
	}

	return func(ctx context.Context, email string) (gmail.API, error) {
		// Look up the account's source type and OAuth app binding via the daemon.
		accounts, err := st.GetCLIAccounts(ctx)
		if err != nil {
			return nil, fmt.Errorf("lookup account %s: %w", email, err)
		}
		var account *daemonclient.CLIAccount
		for i := range accounts {
			if accounts[i].Email == email {
				account = &accounts[i]
				break
			}
		}
		if account == nil {
			return nil, fmt.Errorf("account %s not found in database", email)
		}

		switch account.Type {
		case sourceTypeIMAP:
			return buildIMAPAPIClient(ctx, account.Email, account.SyncConfig)

		case sourceTypeGmail, "":
			if !oauthConfigured {
				return nil, fmt.Errorf("Gmail OAuth not configured for %s: %v", email, secretsErr)
			}

			// Resolve the correct OAuth app for this account
			appSecrets := secretsPath
			if account.OAuthApp != "" {
				appSecrets, err = cfg.OAuth.ClientSecretsFor(account.OAuthApp)
				if err != nil {
					return nil, fmt.Errorf("OAuth app %q: %w", account.OAuthApp, err)
				}
			}

			oauthMgr, err := oauth.NewManager(appSecrets, cfg.TokensDir(), logger)
			if err != nil {
				return nil, fmt.Errorf("create OAuth manager: %w", err)
			}

			tokenSource, err := oauthMgr.TokenSource(ctx, email)
			if err != nil {
				return nil, fmt.Errorf("get token for %s: %w (run 'msgvault add-account %s' first)", email, err, email)
			}

			rateLimiter := gmail.NewRateLimiter(float64(cfg.Sync.RateLimitQPS))
			client := gmail.NewClient(tokenSource,
				gmail.WithLogger(logger),
				gmail.WithRateLimiter(rateLimiter),
			)

			return client, nil

		default:
			return nil, fmt.Errorf("account %s has source type %q, which does not support draft/label operations", email, account.Type)
		}
	}
}

// buildGoogleDocsFactory returns a GoogleDocsClientFactory for configured
// Drive folder sources. It returns nil when no Google Docs sources are enabled.
func buildGoogleDocsFactory() mcpserver.GoogleDocsClientFactory {
	sources := cfg.EnabledGoogleDocsSources()
	if len(sources) == 0 {
		return nil
	}

	return func(ctx context.Context) (googledocs.Client, error) {
		services := make([]googledocs.SourceServices, 0, len(sources))
		for _, src := range sources {
			if err := googledocs.ValidateSource(src); err != nil {
				return nil, err
			}
			clientSecrets, err := cfg.OAuth.ClientSecretsFor(src.OAuthApp)
			if err != nil {
				return nil, fmt.Errorf("OAuth for google-docs source %q: %w", src.Name, err)
			}
			mgr, err := newGoogleDocsDriveOAuthManager(clientSecrets)
			if err != nil {
				return nil, fmt.Errorf("create OAuth manager for google-docs source %q: %w", src.Name, err)
			}
			if !googleDocsDriveTokenReady(mgr, src.GoogleAccount) {
				return nil, fmt.Errorf("no Google Docs OAuth token with Drive/Docs scopes for %s; run 'msgvault add-google-docs-drive %s --folder-id %s --google-account %s' on a machine with browser auth first",
					src.GoogleAccount, src.Name, src.FolderID, src.GoogleAccount)
			}
			ts, err := mgr.TokenSource(ctx, src.GoogleAccount)
			if err != nil {
				return nil, fmt.Errorf("get token for google-docs source %q (%s): %w", src.Name, src.GoogleAccount, err)
			}
			driveService, err := drive.NewService(ctx, option.WithTokenSource(ts))
			if err != nil {
				return nil, fmt.Errorf("create Drive service for google-docs source %q: %w", src.Name, err)
			}
			docsService, err := docs.NewService(ctx, option.WithTokenSource(ts))
			if err != nil {
				return nil, fmt.Errorf("create Docs service for google-docs source %q: %w", src.Name, err)
			}
			services = append(services, googledocs.SourceServices{
				Source: src,
				Drive:  driveService,
				Docs:   docsService,
			})
		}
		return googledocs.NewClient(services)
	}
}

func init() {
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.Flags().BoolVar(&mcpForceSQL, "force-sql", false, "Deprecated in 0.17.0: set [analytics].engine = \"sql\" in config.toml")
	mcpCmd.Flags().BoolVar(&mcpNoSQLiteScanner, "no-sqlite-scanner", false, "Deprecated in 0.17.0: cache engine selection is daemon-managed")
	mcpCmd.Flags().StringVar(&mcpHTTPAddr, "http", "",
		"Serve over StreamableHTTP on this address (e.g. 127.0.0.1:8080) "+
			"instead of stdio. Bare port forms (':8080', '8080') bind to "+
			"loopback only; non-loopback hosts require [server].api_key or "+
			"--http-allow-insecure.")
	mcpCmd.Flags().BoolVar(&mcpHTTPAllowInsecure, "http-allow-insecure", false,
		"Allow --http to bind a non-loopback address without [server].api_key. "+
			"Any configured key still requires bearer authentication. Without a "+
			"key, any reachable client can read your archive; only set this behind "+
			"a trusted network boundary or authenticating reverse proxy.")
	_ = mcpCmd.Flags().MarkDeprecated("force-sql", "deprecated in 0.17.0; set [analytics].engine = \"sql\" in config.toml")
	_ = mcpCmd.Flags().MarkDeprecated("no-sqlite-scanner", "deprecated in 0.17.0; cache engine selection is daemon-managed; use [analytics].engine = \"sql\" for live SQL")
	_ = mcpCmd.Flags().MarkHidden("force-sql")
	_ = mcpCmd.Flags().MarkHidden("no-sqlite-scanner")
}

// normalizeMCPHTTPAddr canonicalises a --http argument and rejects values
// that would expose an unauthenticated MCP server on a non-loopback interface
// unless the user has configured authentication or explicitly opted in.
//
// Forms accepted:
//   - "8080"            → "127.0.0.1:8080" (loopback)
//   - ":8080"           → "127.0.0.1:8080" (loopback; Go's default would be
//     all-interfaces, which is the footgun this guards against)
//   - "127.0.0.1:8080"  → unchanged (loopback, allowed)
//   - "[::1]:8080"      → unchanged (loopback, allowed)
//   - "192.168.1.5:8080", "0.0.0.0:8080", "vault.local:8080" → rejected
//     unless [server].api_key or --http-allow-insecure is set
func normalizeMCPHTTPAddr(addr string, allowInsecure, authenticated bool) (string, error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return "", errors.New("--http requires an address")
	}

	// Bare port: "8080" or ":8080".
	if !strings.Contains(trimmed, ":") {
		if _, convErr := strconv.Atoi(trimmed); convErr == nil {
			return "127.0.0.1:" + trimmed, nil
		}
		return "", fmt.Errorf(
			"--http %q: not a port and not host:port", trimmed)
	}
	if strings.HasPrefix(trimmed, ":") {
		return "127.0.0.1" + trimmed, nil
	}

	host, _, splitErr := net.SplitHostPort(trimmed)
	if splitErr != nil {
		return "", fmt.Errorf("--http %q: %w", trimmed, splitErr)
	}

	if isLoopbackHost(host) {
		return trimmed, nil
	}
	if !authenticated && !allowInsecure {
		return "", fmt.Errorf(
			"--http %q: refusing to bind a non-loopback address without "+
				"[server].api_key or --http-allow-insecure (configure an API key "+
				"for bearer authentication, or only opt into unauthenticated "+
				"access behind a trusted network boundary)", trimmed)
	}
	return trimmed, nil
}

// isLoopbackHost reports whether host resolves to a loopback address.
// Empty host is NOT treated as loopback: net.Listen on a host:port pair
// with an empty host binds to all interfaces, which is the exact footgun
// this guard exists to catch (e.g. "[]:8080" passes net.SplitHostPort
// with an empty host but binds to all-interfaces).
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

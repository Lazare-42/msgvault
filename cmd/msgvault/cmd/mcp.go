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
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/googledocs"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

var mcpForceSQL bool
var mcpNoSQLiteScanner bool
var mcpHTTPAddr string
var mcpHTTPAllowInsecure bool

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run MCP server for Claude Desktop integration",
	Long: `Start an MCP (Model Context Protocol) server over stdio.

This allows Claude Desktop (or any MCP client) to query your email archive
using tools like search_messages, get_message, list_messages, get_stats,
aggregate, stage_deletion, and draft management (list, create, update, send,
delete drafts).

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
		env, err := setupMCPEnv(mcpForceSQL, mcpNoSQLiteScanner)
		if err != nil {
			return err
		}
		defer env.Close()

		// Derive from cmd.Context() so signal handling installed by
		// the cobra root command (SIGINT/SIGTERM → ctx.Done()) reaches
		// the MCP transport and can trigger ServeHTTPWithOptions's
		// graceful shutdown.
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		// Build optional vector-search components. MCP runs as a
		// query-only server, so the worker and enqueuer fields go
		// unused — only Backend, HybridEngine, and VectorCfg reach
		// the MCP layer.
		dbPath := cfg.DatabaseDSN()
		vf, err := setupVectorFeatures(ctx, env.Store.DB(), dbPath, true)
		if err != nil {
			return fmt.Errorf("vector features: %w", err)
		}
		defer func() {
			if vf != nil && vf.Close != nil {
				if closeErr := vf.Close(); closeErr != nil {
					logger.Warn("closing vectors.db failed", "error", closeErr)
				}
			}
		}()

		opts := mcpserver.ServeOptions{
			Engine:            env.Engine,
			AttachmentsDir:    cfg.AttachmentsDir(),
			DataDir:           cfg.Data.DataDir,
			GmailFactory:      env.GmailFactory,
			GoogleDocsFactory: env.GoogleDocsFactory,
		}
		if vf != nil {
			opts.HybridEngine = vf.HybridEngine
			opts.Backend = vf.Backend
			opts.VectorCfg = vf.Cfg
		}

		if mcpHTTPAddr != "" {
			normalized, err := normalizeMCPHTTPAddr(mcpHTTPAddr, mcpHTTPAllowInsecure)
			if err != nil {
				return usageErr(cmd, err)
			}
			return mcpserver.ServeHTTPWithOptions(ctx, opts, normalized)
		}
		return mcpserver.ServeWithOptions(ctx, opts)
	},
}

// mcpShouldUseParquet reports whether the MCP server should use the
// DuckDB/Parquet engine. This is the SQLite-only branch of the engine
// selection: PostgreSQL stores must be handled by the caller before this
// is consulted (the Parquet cache is a SQLite → DuckDB ETL with no
// PostgreSQL meaning). It returns true only when the user has not forced
// SQLite and a complete Parquet cache exists.
func mcpShouldUseParquet(forceSQL bool, analyticsDir string) bool {
	return !forceSQL && query.HasCompleteParquetData(analyticsDir)
}

// mcpEnv holds shared resources for MCP commands (stdio, HTTP, SSE).
type mcpEnv struct {
	Store             *store.Store
	Engine            query.Engine
	GmailFactory      mcpserver.GmailClientFactory
	GoogleDocsFactory mcpserver.GoogleDocsClientFactory
	duckEngine        *query.DuckDBEngine // non-nil when DuckDB is used
}

// Close releases all resources held by the MCP environment.
func (e *mcpEnv) Close() {
	if e.duckEngine != nil {
		_ = e.duckEngine.Close()
	}
	_ = e.Store.Close()
}

// setupMCPEnv initializes the database, query engine, and Gmail factory
// shared by all MCP transport commands (stdio, HTTP, SSE).
//
// The database is opened read-only: MCP is a query-only workload. This
// avoids SQLite write-lock contention when multiple MCP processes (one per
// Claude Code session) access the same database. Schema migrations and FTS
// backfill are write operations handled by init-db / sync / tui — not by MCP.
func setupMCPEnv(forceSQL, noSQLiteScanner bool) (*mcpEnv, error) {
	dbPath := cfg.DatabaseDSN()

	s, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if stale, col, err := s.SchemaStale(); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("check schema: %w", err)
	} else if stale {
		_ = s.Close()
		return nil, fmt.Errorf(
			"database schema is outdated (missing %s); "+
				"run 'msgvault init-db' to update", col)
	}

	if s.FTS5Available() && s.NeedsFTSBackfill() {
		fmt.Fprintf(os.Stderr,
			"Warning: full-text search index needs populating; "+
				"body-text search will return incomplete results "+
				"until 'msgvault tui' or 'msgvault search' is run\n")
	}

	env := &mcpEnv{Store: s}
	analyticsDir := cfg.AnalyticsDir()

	// The Parquet analytics cache is a SQLite → DuckDB ETL and has no
	// meaning when the system of record is PostgreSQL: the cache may be
	// stale relative to PG, and NewDuckDBEngine would receive the
	// PostgreSQL DSN/handle in its SQLite slots, routing SQLite-specific
	// queries through a PG connection. On PG, skip the cache entirely and
	// use the dialect-aware engine directly (mirrors serve.go / tui.go).
	if s.IsPostgreSQL() {
		env.Engine = query.NewEngine(s.DB(), true)
	} else if mcpShouldUseParquet(forceSQL, analyticsDir) {
		var duckOpts query.DuckDBOptions
		if noSQLiteScanner {
			duckOpts.DisableSQLiteScanner = true
		}
		duckEngine, err := query.NewDuckDBEngine(analyticsDir, dbPath, s.DB(), duckOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to open Parquet engine: %v\n", err)
			fmt.Fprintf(os.Stderr, "Falling back to SQLite\n")
			env.Engine = query.NewEngine(s.DB(), false)
		} else {
			env.Engine = duckEngine
			env.duckEngine = duckEngine
		}
	} else {
		env.Engine = query.NewEngine(s.DB(), false)
	}

	env.GmailFactory = buildGmailFactory(s)
	env.GoogleDocsFactory = buildGoogleDocsFactory()
	return env, nil
}

// buildGmailFactory returns a GmailClientFactory that creates authenticated
// Gmail clients using OAuth tokens. Returns nil if OAuth is not configured.
func buildGmailFactory(s *store.Store) mcpserver.GmailClientFactory {
	// Check if OAuth secrets are available
	secretsPath, err := cfg.OAuth.ClientSecretsFor("")
	if err != nil {
		// OAuth not configured — draft tools will be disabled
		fmt.Fprintf(os.Stderr, "Note: OAuth not configured, draft tools disabled\n")
		return nil
	}

	return func(ctx context.Context, email string) (*gmail.Client, error) {
		// Look up the account's OAuth app binding
		src, err := s.GetSourceByIdentifier(email)
		if err != nil {
			return nil, fmt.Errorf("lookup account %s: %w", email, err)
		}
		if src == nil {
			return nil, fmt.Errorf("account %s not found in database", email)
		}

		// Resolve the correct OAuth app for this account
		appName := sourceOAuthApp(src)
		appSecrets := secretsPath
		if appName != "" {
			appSecrets, err = cfg.OAuth.ClientSecretsFor(appName)
			if err != nil {
				return nil, fmt.Errorf("OAuth app %q: %w", appName, err)
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
	mcpCmd.Flags().BoolVar(&mcpForceSQL, "force-sql", false, "Force SQLite queries instead of Parquet")
	mcpCmd.Flags().BoolVar(&mcpNoSQLiteScanner, "no-sqlite-scanner", false, "Disable DuckDB sqlite_scanner extension (use direct SQLite fallback)")
	mcpCmd.Flags().StringVar(&mcpHTTPAddr, "http", "",
		"Serve over StreamableHTTP on this address (e.g. 127.0.0.1:8080) "+
			"instead of stdio. Bare port forms (':8080', '8080') bind to "+
			"loopback only; non-loopback hosts require --http-allow-insecure.")
	mcpCmd.Flags().BoolVar(&mcpHTTPAllowInsecure, "http-allow-insecure", false,
		"Allow --http to bind a non-loopback address. The MCP server has no "+
			"built-in authentication, so any reachable client can read your "+
			"archive. Only set this on trusted networks (Tailscale, "+
			"VPN-only) or behind an authenticating reverse proxy.")
	_ = mcpCmd.Flags().MarkHidden("no-sqlite-scanner")
}

// normalizeMCPHTTPAddr canonicalises a --http argument and rejects values
// that would expose the unauthenticated MCP server on a non-loopback
// interface unless the user has explicitly opted in.
//
// Forms accepted:
//   - "8080"            → "127.0.0.1:8080" (loopback)
//   - ":8080"           → "127.0.0.1:8080" (loopback; Go's default would be
//     all-interfaces, which is the footgun this guards against)
//   - "127.0.0.1:8080"  → unchanged (loopback, allowed)
//   - "[::1]:8080"      → unchanged (loopback, allowed)
//   - "192.168.1.5:8080", "0.0.0.0:8080", "vault.local:8080" → rejected
//     unless --http-allow-insecure is set
func normalizeMCPHTTPAddr(addr string, allowInsecure bool) (string, error) {
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
	if !allowInsecure {
		return "", fmt.Errorf(
			"--http %q: refusing to bind a non-loopback address without "+
				"--http-allow-insecure (the MCP server has no built-in "+
				"authentication; only opt in on trusted networks or "+
				"behind an authenticating reverse proxy)", trimmed)
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

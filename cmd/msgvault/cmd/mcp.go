package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/gmail"
	mcpserver "github.com/wesm/msgvault/internal/mcp"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/store"
)

var mcpForceSQL bool
var mcpNoSQLiteScanner bool

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
		dbPath := cfg.DatabaseDSN()

		// Open read-only: MCP is a query-only workload. This avoids
		// SQLite write-lock contention when multiple MCP processes
		// (one per Claude Code session) access the same database.
		// Schema migrations and FTS backfill are write operations
		// handled by init-db / sync / tui — not by MCP.
		s, err := store.OpenReadOnly(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = s.Close() }()

		if stale, col, err := s.SchemaStale(); err != nil {
			return fmt.Errorf("check schema: %w", err)
		} else if stale {
			return fmt.Errorf(
				"database schema is outdated (missing %s); "+
					"run 'msgvault init-db' to update", col)
		}

		if s.FTS5Available() && s.NeedsFTSBackfill() {
			fmt.Fprintf(os.Stderr,
				"Warning: full-text search index needs populating; "+
					"body-text search will return incomplete results "+
					"until 'msgvault tui' or 'msgvault search' is run\n")
		}

		var engine query.Engine
		analyticsDir := cfg.AnalyticsDir()

		if !mcpForceSQL && query.HasCompleteParquetData(analyticsDir) {
			var duckOpts query.DuckDBOptions
			if mcpNoSQLiteScanner {
				duckOpts.DisableSQLiteScanner = true
			}
			duckEngine, err := query.NewDuckDBEngine(analyticsDir, dbPath, s.DB(), duckOpts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Failed to open Parquet engine: %v\n", err)
				fmt.Fprintf(os.Stderr, "Falling back to SQLite\n")
				engine = query.NewSQLiteEngine(s.DB())
			} else {
				engine = duckEngine
				defer func() { _ = duckEngine.Close() }()
			}
		} else {
			engine = query.NewSQLiteEngine(s.DB())
		}

		// Set up Gmail client factory for draft operations.
		// If OAuth is not configured, draft tools are simply not exposed.
		var gmailFactory mcpserver.GmailClientFactory
		gmailFactory = buildGmailFactory(s)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Build optional vector-search components. MCP runs as a
		// query-only server, so the worker and enqueuer fields go
		// unused — only Backend, HybridEngine, and VectorCfg reach
		// the MCP layer.
		vf, err := setupVectorFeatures(ctx, s.DB(), dbPath)
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
			Engine:         engine,
			AttachmentsDir: cfg.AttachmentsDir(),
			DataDir:        cfg.Data.DataDir,
			GmailFactory:   gmailFactory,
		}
		if vf != nil {
			opts.HybridEngine = vf.HybridEngine
			opts.Backend = vf.Backend
			opts.VectorCfg = vf.Cfg
		}
		return mcpserver.ServeWithOptions(ctx, opts)
	},
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

func init() {
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.Flags().BoolVar(&mcpForceSQL, "force-sql", false, "Force SQLite queries instead of Parquet")
	mcpCmd.Flags().BoolVar(&mcpNoSQLiteScanner, "no-sqlite-scanner", false, "Disable DuckDB sqlite_scanner extension (use direct SQLite fallback)")
	_ = mcpCmd.Flags().MarkHidden("no-sqlite-scanner")
}

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	mcpserver "github.com/wesm/msgvault/internal/mcp"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/store"
)

var mcpSSEAddr string
var mcpSSEForceSQL bool
var mcpSSENoSQLiteScanner bool

var mcpSSECmd = &cobra.Command{
	Use:   "mcp-sse",
	Short: "Run MCP server over SSE/HTTP for remote clients",
	Long: `Start an MCP (Model Context Protocol) server over SSE (Server-Sent Events).

This allows remote MCP clients to connect over HTTP instead of stdio,
suitable for containerized deployments (e.g., Kubernetes).

Clients connect to http://<addr>/sse for the SSE event stream.

Example Claude Desktop config (with port-forward):
  {
    "mcpServers": {
      "msgvault": {
        "url": "http://localhost:8080/sse"
      }
    }
  }`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath := cfg.DatabaseDSN()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer s.Close()

		if err := s.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}

		if s.NeedsFTSBackfill() {
			go func() {
				_, _ = s.BackfillFTS(nil)
			}()
		}

		var engine query.Engine
		analyticsDir := cfg.AnalyticsDir()

		if !mcpSSEForceSQL && query.HasCompleteParquetData(analyticsDir) {
			var duckOpts query.DuckDBOptions
			if mcpSSENoSQLiteScanner {
				duckOpts.DisableSQLiteScanner = true
			}
			duckEngine, err := query.NewDuckDBEngine(analyticsDir, dbPath, s.DB(), duckOpts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Failed to open Parquet engine: %v\n", err)
				fmt.Fprintf(os.Stderr, "Falling back to SQLite\n")
				engine = query.NewSQLiteEngine(s.DB())
			} else {
				engine = duckEngine
				defer duckEngine.Close()
			}
		} else {
			engine = query.NewSQLiteEngine(s.DB())
		}

		var gmailFactory mcpserver.GmailClientFactory
		gmailFactory = buildGmailFactory(s)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Graceful shutdown on SIGINT/SIGTERM
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintf(os.Stderr, "Shutting down MCP SSE server...\n")
			cancel()
		}()

		fmt.Fprintf(os.Stderr, "Starting MCP SSE server on %s\n", mcpSSEAddr)
		return mcpserver.ServeSSE(ctx, mcpSSEAddr, mcpserver.ServeOptions{
			Engine:         engine,
			AttachmentsDir: cfg.AttachmentsDir(),
			DataDir:        cfg.Data.DataDir,
			GmailFactory:   gmailFactory,
		})
	},
}

func init() {
	rootCmd.AddCommand(mcpSSECmd)
	mcpSSECmd.Flags().StringVar(&mcpSSEAddr, "addr", "0.0.0.0:8080", "Address to listen on (host:port)")
	mcpSSECmd.Flags().BoolVar(&mcpSSEForceSQL, "force-sql", false, "Force SQLite queries instead of Parquet")
	mcpSSECmd.Flags().BoolVar(&mcpSSENoSQLiteScanner, "no-sqlite-scanner", false, "Disable DuckDB sqlite_scanner extension")
	_ = mcpSSECmd.Flags().MarkHidden("no-sqlite-scanner")
}

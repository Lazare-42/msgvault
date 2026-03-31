package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
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
		st, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return fmt.Errorf("open daemon: %w", err)
		}
		defer func() { _ = st.Close() }()

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		opts, err := daemonMCPServeOptions(ctx, st)
		if err != nil {
			return err
		}
		opts.GmailFactory = buildGmailFactory(st)

		// Graceful shutdown on SIGINT/SIGTERM
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintf(os.Stderr, "Shutting down MCP SSE server...\n")
			cancel()
		}()

		fmt.Fprintf(os.Stderr, "Starting MCP SSE server on %s\n", mcpSSEAddr)
		return mcpserver.ServeSSE(ctx, mcpSSEAddr, opts)
	},
}

func init() {
	rootCmd.AddCommand(mcpSSECmd)
	mcpSSECmd.Flags().StringVar(&mcpSSEAddr, "addr", "0.0.0.0:8080", "Address to listen on (host:port)")
	mcpSSECmd.Flags().BoolVar(&mcpSSEForceSQL, "force-sql", false, "Deprecated in 0.17.0: cache engine selection is daemon-managed")
	mcpSSECmd.Flags().BoolVar(&mcpSSENoSQLiteScanner, "no-sqlite-scanner", false, "Deprecated in 0.17.0: cache engine selection is daemon-managed")
	_ = mcpSSECmd.Flags().MarkDeprecated("force-sql", "deprecated in 0.17.0; cache engine selection is daemon-managed")
	_ = mcpSSECmd.Flags().MarkDeprecated("no-sqlite-scanner", "deprecated in 0.17.0; cache engine selection is daemon-managed")
	_ = mcpSSECmd.Flags().MarkHidden("force-sql")
	_ = mcpSSECmd.Flags().MarkHidden("no-sqlite-scanner")
}

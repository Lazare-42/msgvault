package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	mcpserver "github.com/wesm/msgvault/internal/mcp"
)

var mcpHTTPAddr string
var mcpHTTPForceSQL bool
var mcpHTTPNoSQLiteScanner bool
var mcpHTTPAPIKey string
var mcpHTTPAllowInsecure bool

var mcpHTTPCmd = &cobra.Command{
	Use:     "mcp-http",
	Aliases: []string{"mcp-sse"},
	Short:   "Run MCP server over HTTP for remote clients",
	Long: `Start an MCP (Model Context Protocol) server over Streamable HTTP.

This allows remote MCP clients to connect over HTTP instead of stdio,
suitable for containerized deployments (e.g., Kubernetes).

Clients connect to http://<addr>/mcp for the Streamable HTTP endpoint.

Security: When listening on a non-loopback address, an --api-key is required
unless --allow-insecure is set. The intended deployment model is
kubectl port-forward (loopback) or a network policy restricting access.

Example Claude Desktop config (with port-forward):
  {
    "mcpServers": {
      "msgvault": {
        "url": "http://localhost:8080/mcp"
      }
    }
  }`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate security: require API key for non-loopback addresses
		host, _, _ := net.SplitHostPort(mcpHTTPAddr)
		ip := net.ParseIP(host)
		isLoopback := host == "" || host == "localhost" || (ip != nil && ip.IsLoopback())
		if !isLoopback && mcpHTTPAPIKey == "" && !mcpHTTPAllowInsecure {
			return fmt.Errorf("refusing to start: bind address %q is not loopback and no --api-key is set\n\n"+
				"Set --api-key <key>, or --allow-insecure to override", mcpHTTPAddr)
		}
		if !isLoopback && mcpHTTPAPIKey == "" {
			fmt.Fprintf(os.Stderr, "WARNING: MCP HTTP server listening on %s with no authentication\n", mcpHTTPAddr)
		}

		env, err := setupMCPEnv(mcpHTTPForceSQL, mcpHTTPNoSQLiteScanner)
		if err != nil {
			return err
		}
		defer env.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Graceful shutdown on SIGINT/SIGTERM
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintf(os.Stderr, "Shutting down MCP HTTP server...\n")
			cancel()
		}()

		fmt.Fprintf(os.Stderr, "Starting MCP HTTP server on %s\n", mcpHTTPAddr)
		return mcpserver.ServeStreamableHTTP(ctx, mcpHTTPAddr, mcpHTTPAPIKey, mcpserver.ServeOptions{
			Engine:         env.Engine,
			AttachmentsDir: cfg.AttachmentsDir(),
			DataDir:        cfg.Data.DataDir,
			GmailFactory:   env.GmailFactory,
		})
	},
}

func init() {
	rootCmd.AddCommand(mcpHTTPCmd)
	mcpHTTPCmd.Flags().StringVar(&mcpHTTPAddr, "addr", "0.0.0.0:8080", "Address to listen on (host:port)")
	mcpHTTPCmd.Flags().StringVar(&mcpHTTPAPIKey, "api-key", "", "API key for bearer token authentication (required for non-loopback)")
	mcpHTTPCmd.Flags().BoolVar(&mcpHTTPAllowInsecure, "allow-insecure", false, "Allow unauthenticated access on non-loopback addresses")
	mcpHTTPCmd.Flags().BoolVar(&mcpHTTPForceSQL, "force-sql", false, "Force SQLite queries instead of Parquet")
	mcpHTTPCmd.Flags().BoolVar(&mcpHTTPNoSQLiteScanner, "no-sqlite-scanner", false, "Disable DuckDB sqlite_scanner extension")
	_ = mcpHTTPCmd.Flags().MarkHidden("no-sqlite-scanner")
}

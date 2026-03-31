package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var mcpSSEAddr string
var mcpSSEForceSQL bool
var mcpSSENoSQLiteScanner bool
var mcpSSEAPIKey string
var mcpSSEAllowInsecure bool

var mcpSSECmd = &cobra.Command{
	Use:   "mcp-sse",
	Short: "Run MCP server over HTTP for remote clients",
	Long: `Start an MCP (Model Context Protocol) server over Streamable HTTP.

This allows remote MCP clients to connect over HTTP instead of stdio,
suitable for containerized deployments (e.g., Kubernetes).

Clients connect to http://<addr>/mcp for the Streamable HTTP endpoint.

Security: When listening on a non-loopback address, an --api-key is required
unless --allow-insecure is set. Use SSH port-forwarding as an alternative.

Example Claude Desktop config (with port-forward):
  {
    "mcpServers": {
      "msgvault": {
        "url": "http://localhost:8080/mcp"
      }
    }
  }`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate security: require an API key for non-loopback addresses
		// unless the operator has explicitly opted into insecure exposure.
		host, _, _ := net.SplitHostPort(mcpSSEAddr)
		if !isLoopbackHost(host) {
			if mcpSSEAPIKey == "" && !mcpSSEAllowInsecure {
				return fmt.Errorf(
					"refusing to start: bind address %q is not loopback and no --api-key is set\n\n"+
						"Set --api-key <key>, or --allow-insecure to override", mcpSSEAddr)
			}
			if mcpSSEAPIKey == "" {
				fmt.Fprintf(os.Stderr,
					"WARNING: MCP HTTP server listening on %s with no authentication\n", mcpSSEAddr)
			}
		}

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
			fmt.Fprintf(os.Stderr, "Shutting down MCP HTTP server...\n")
			cancel()
		}()

		fmt.Fprintf(os.Stderr, "Starting MCP HTTP server on %s\n", mcpSSEAddr)
		return serveMCPHTTPWithOptions(ctx, opts, mcpSSEAddr, mcpSSEAPIKey)
	},
}

func init() {
	rootCmd.AddCommand(mcpSSECmd)
	mcpSSECmd.Flags().StringVar(&mcpSSEAddr, "addr", "0.0.0.0:8080", "Address to listen on (host:port)")
	mcpSSECmd.Flags().StringVar(&mcpSSEAPIKey, "api-key", "", "API key for bearer token authentication (required for non-loopback)")
	mcpSSECmd.Flags().BoolVar(&mcpSSEAllowInsecure, "allow-insecure", false, "Allow unauthenticated access on non-loopback addresses")
	mcpSSECmd.Flags().BoolVar(&mcpSSEForceSQL, "force-sql", false, "Deprecated in 0.17.0: cache engine selection is daemon-managed")
	mcpSSECmd.Flags().BoolVar(&mcpSSENoSQLiteScanner, "no-sqlite-scanner", false, "Deprecated in 0.17.0: cache engine selection is daemon-managed")
	_ = mcpSSECmd.Flags().MarkDeprecated("force-sql", "deprecated in 0.17.0; cache engine selection is daemon-managed")
	_ = mcpSSECmd.Flags().MarkDeprecated("no-sqlite-scanner", "deprecated in 0.17.0; cache engine selection is daemon-managed")
	_ = mcpSSECmd.Flags().MarkHidden("force-sql")
	_ = mcpSSECmd.Flags().MarkHidden("no-sqlite-scanner")
}

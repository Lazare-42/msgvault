package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query"
	whatsapplive "go.kenn.io/msgvault/internal/whatsapp/live"
)

var whatsappLinkPhone string
var whatsappLiveMCPAddr string

var whatsappLinkCmd = &cobra.Command{
	Use:   "whatsapp-link",
	Short: "Link a WhatsApp account for live MCP sending",
	RunE: func(cmd *cobra.Command, args []string) error {
		transport, err := openWhatsAppTransport(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = transport.Close() }()

		if whatsappLinkPhone != "" {
			return transport.PairPhone(cmd.Context(), whatsappLinkPhone, os.Stdout)
		}
		return transport.LinkQR(cmd.Context(), os.Stdout)
	},
}

var whatsappStatusCmd = &cobra.Command{
	Use:   "whatsapp-status",
	Short: "Show live WhatsApp pairing status",
	RunE: func(cmd *cobra.Command, args []string) error {
		transport, err := openWhatsAppTransport(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = transport.Close() }()

		status, err := transport.Status(cmd.Context())
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	},
}

var whatsappLiveMCPCmd = &cobra.Command{
	Use:   "whatsapp-live-mcp",
	Short: "Run writable live WhatsApp MCP server",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, err := normalizeMCPHTTPAddr(whatsappLiveMCPAddr, false, cfg.Server.APIKey != "")
		if err != nil {
			return usageErr(cmd, err)
		}

		st, cleanup, err := openWritableStoreAndInit()
		if err != nil {
			return err
		}
		defer cleanup()

		transport, err := openWhatsAppTransport(cmd.Context())
		if err != nil {
			return err
		}
		service, err := whatsapplive.NewService(whatsapplive.ServiceOptions{
			Store:     st,
			Transport: transport,
		})
		if err != nil {
			_ = transport.Close()
			return err
		}
		transport.SetInboundHandler(func(ctx context.Context, msg whatsapplive.InboundMessage) error {
			_, archiveErr := service.ArchiveInbound(ctx, msg)
			return archiveErr
		})
		defer func() { _ = service.Close() }()

		if err := service.Connect(cmd.Context()); err != nil {
			return fmt.Errorf("connect WhatsApp: %w", err)
		}

		engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
		opts := mcpserver.ServeOptions{
			Engine:         engine,
			AttachmentsDir: cfg.AttachmentsDir(),
			DataDir:        cfg.Data.DataDir,
			WhatsAppFactory: func(ctx context.Context, account string) (whatsapplive.Client, error) {
				return service, nil
			},
		}
		return mcpserver.ServeHTTPWithOptions(cmd.Context(), opts, addr, cfg.Server.APIKey)
	},
}

func openWhatsAppTransport(ctx context.Context) (*whatsapplive.WhatsmeowTransport, error) {
	sessionPath := filepath.Join(cfg.Data.DataDir, "whatsapp-session.db")
	return whatsapplive.NewWhatsmeowTransport(ctx, whatsapplive.WhatsmeowOptions{
		SessionPath: sessionPath,
	})
}

func init() {
	rootCmd.AddCommand(whatsappLinkCmd)
	rootCmd.AddCommand(whatsappStatusCmd)
	rootCmd.AddCommand(whatsappLiveMCPCmd)

	whatsappLinkCmd.Flags().StringVar(&whatsappLinkPhone, "phone", "", "International phone number for pairing code login instead of QR")
	whatsappLiveMCPCmd.Flags().StringVar(&whatsappLiveMCPAddr, "addr", "127.0.0.1:8121", "Loopback address to listen on (host:port)")
}

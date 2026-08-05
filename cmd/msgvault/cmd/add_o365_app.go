package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
)

var (
	o365AppTenantID string
	o365AppClientID string
	o365AppCertPath string
)

// o365AppIMAPHost is the organizational Exchange Online IMAP endpoint. App-only
// (IMAP.AccessAsApp) is only available for organizational mailboxes, so unlike
// the delegated add-o365 flow there is no personal-account host to detect.
const o365AppIMAPHost = "outlook.office365.com"

func newAddO365AppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-o365-app <mailbox>",
		Short: "Add a Microsoft 365 shared mailbox via certificate app-only auth",
		Long: `Add a Microsoft 365 mailbox using certificate-based app-only
(client-credentials) authentication — the unattended-service path.

Unlike add-o365 (per-user browser consent + refresh token), this uses an Entra
app registration granted the IMAP.AccessAsApp application permission and scoped
to a single mailbox via Exchange Online RBAC. No browser, no user, no refresh
token: a signed JWT client assertion is exchanged for an access token on demand.

Prerequisites (done once by a tenant admin):
  1. Entra app registration with a certificate credential uploaded.
  2. API permission Office 365 Exchange Online → IMAP.AccessAsApp (admin consent).
  3. Grant the app's service principal access to the mailbox, e.g.:
       New-ServicePrincipal -AppId <app-id> -ServiceId <object-id>
       Add-MailboxPermission -Identity <mailbox> -User <app-sp> -AccessRights FullAccess

The --cert file is a PEM bundle containing the app's RSA private key AND the
matching certificate (the cert supplies the x5t thumbprint).

Example:
  msgvault add-o365-app hr@contoso.com \
    --tenant <tenant-guid> --client-id <app-id> --cert /path/to/app.pem`,
		Args: cobra.ExactArgs(1),
		RunE: runAddO365App,
	}
	cmd.Flags().StringVar(&o365AppTenantID, "tenant", "", "Entra tenant ID (GUID or verified domain)")
	cmd.Flags().StringVar(&o365AppClientID, "client-id", "", "Entra app registration (client) ID")
	cmd.Flags().StringVar(&o365AppCertPath, "cert", "", "PEM file with the app's private key + certificate")
	_ = cmd.MarkFlagRequired("tenant")
	_ = cmd.MarkFlagRequired("client-id")
	_ = cmd.MarkFlagRequired("cert")
	return cmd
}

func runAddO365App(cmd *cobra.Command, args []string) error {
	mailbox := args[0]

	imapCfg := &imapclient.Config{
		Host:       o365AppIMAPHost,
		Port:       993,
		TLS:        true,
		Username:   mailbox,
		AuthMethod: imapclient.AuthXOAuth2,
		MSAppOnly:  true,
		MSTenantID: o365AppTenantID,
		MSClientID: o365AppClientID,
		MSCertPath: o365AppCertPath,
	}

	// Health-check the credentials before persisting: mint a token and open an
	// authenticated IMAP session. This surfaces a bad cert, missing admin
	// consent, or an unscoped mailbox immediately rather than at first sync.
	fmt.Printf("Verifying app-only access to %s...\n", mailbox)
	tokenFn, err := microsoftIMAPTokenSource(cmd.Context(), imapCfg)
	if err != nil {
		return err
	}
	client := imapclient.NewClient(imapCfg, "", imapclient.WithTokenSource(tokenFn))
	if _, err := client.ListMailboxes(cmd.Context()); err != nil {
		return fmt.Errorf("app-only IMAP verification failed for %s: %w", mailbox, err)
	}

	s, cleanup, err := openWritableStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer cleanup()

	identifier := imapCfg.Identifier()

	var source *store.Source
	existing, err := s.GetSourcesByDisplayName(mailbox)
	if err != nil {
		return fmt.Errorf("look up existing source: %w", err)
	}
	for _, src := range existing {
		if src.SourceType == sourceTypeIMAP && isMicrosoftIMAPSource(src, mailbox) {
			source = src
			break
		}
	}
	if source != nil {
		if err := s.UpdateSourceIdentifier(source.ID, identifier); err != nil {
			return fmt.Errorf("update source identifier: %w", err)
		}
	} else {
		source, err = s.GetOrCreateSource(sourceTypeIMAP, identifier)
		if err != nil {
			return fmt.Errorf("create source: %w", err)
		}
	}

	cfgJSON, err := imapCfg.ToJSON()
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
	}
	if err := s.UpdateSourceSyncConfig(source.ID, cfgJSON); err != nil {
		return fmt.Errorf("store config: %w", err)
	}
	if err := s.UpdateSourceDisplayName(source.ID, mailbox); err != nil {
		return fmt.Errorf("set display name: %w", err)
	}
	if err := runPostSourceCreateMigrations(s); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	fmt.Printf("\nMicrosoft 365 shared mailbox added (app-only)!\n")
	fmt.Printf("  Mailbox:    %s\n", mailbox)
	fmt.Printf("  Identifier: %s\n", identifier)
	fmt.Printf("  Auth:       certificate app-only (no refresh token)\n")
	fmt.Println()
	fmt.Println("You can now run:")
	fmt.Printf("  msgvault sync-full %s\n", mailbox)
	return nil
}

func init() {
	rootCmd.AddCommand(newAddO365AppCmd())
}

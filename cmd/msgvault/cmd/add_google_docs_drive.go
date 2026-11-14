package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/googledocs"
	"go.kenn.io/msgvault/internal/oauth"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
)

func newAddGoogleDocsDriveCmd() *cobra.Command {
	var opts struct {
		FolderID        string
		GoogleAccount   string
		OAuthApp        string
		SkipAuthForTest bool
	}
	cmd := &cobra.Command{
		Use:   "add-google-docs-drive <name>",
		Short: "Configure a Google Drive folder of Google Docs for MCP query and update",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			cfg := state.cfg
			if opts.FolderID == "" {
				return errors.New("--folder-id is required")
			}
			if opts.GoogleAccount == "" {
				return errors.New("--google-account is required")
			}
			name := args[0]
			if cfg.GetGoogleDocsSource(name) != nil {
				return fmt.Errorf("google-docs source %q already exists", name)
			}
			src := config.GoogleDocsSource{
				Name:          name,
				Enabled:       true,
				FolderID:      opts.FolderID,
				GoogleAccount: opts.GoogleAccount,
				OAuthApp:      opts.OAuthApp,
			}
			if err := googledocs.ValidateSource(src); err != nil {
				return err
			}
			if !opts.SkipAuthForTest {
				if err := ensureGoogleDocsDriveToken(cmd.Context(), opts.GoogleAccount, opts.OAuthApp); err != nil {
					return err
				}
			}
			cfg.GoogleDocs.Sources = append(cfg.GoogleDocs.Sources, src)
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			if !opts.SkipAuthForTest {
				cmd.Println("Google Docs Drive source configured.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.FolderID, "folder-id", "", "Google Drive folder ID containing Google Docs")
	cmd.Flags().StringVar(&opts.GoogleAccount, "google-account", "", "Google account email used for Drive/Docs OAuth token lookup")
	cmd.Flags().StringVar(&opts.OAuthApp, "oauth-app", "", "Named OAuth app from config.toml")
	cmd.Flags().BoolVar(&opts.SkipAuthForTest, "skip-auth-for-test", false, "Skip OAuth setup in tests")
	_ = cmd.Flags().MarkHidden("skip-auth-for-test")
	return cmd
}

func ensureGoogleDocsDriveToken(ctx context.Context, googleAccount, oauthApp string) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	clientSecrets, err := cfg.OAuth.CredentialsFor(oauthApp)
	if err != nil {
		return err
	}
	mgr, err := newGoogleDocsDriveOAuthManager(ctx, clientSecrets)
	if err != nil {
		return err
	}
	if googleDocsDriveTokenReady(ctx, mgr, googleAccount) {
		return nil
	}
	return mgr.Authorize(ctx, googleAccount)
}

func newGoogleDocsDriveOAuthManager(ctx context.Context, clientSecrets config.OAuthApp) (*oauth.Manager, error) {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	cfg, logger := state.cfg, state.logger
	// The current OAuth manager validates account identity through Gmail's
	// profile endpoint, so request a read-only Gmail scope alongside Docs/Drive.
	return oauth.NewManagerWithCredentials(ctx, clientSecrets, cfg.TokensDir(), cfg.OAuth.Tokens, logger, googleDocsDriveScopes())
}

func googleDocsDriveScopes() []string {
	return []string{
		drive.DriveReadonlyScope,
		docs.DocumentsScope,
		"https://www.googleapis.com/auth/gmail.readonly",
	}
}

func googleDocsDriveTokenReady(ctx context.Context, mgr *oauth.Manager, googleAccount string) bool {
	if !mgr.HasToken(ctx, googleAccount) {
		return false
	}
	hasDriveRead := mgr.HasScope(ctx, googleAccount, drive.DriveReadonlyScope) || mgr.HasScope(ctx, googleAccount, drive.DriveScope)
	return hasDriveRead && mgr.HasScope(ctx, googleAccount, docs.DocumentsScope)
}

func init() {
	rootCmd.AddCommand(newAddGoogleDocsDriveCmd())
}

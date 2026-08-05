package cmd

import (
	"context"
	"errors"
	"fmt"

	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
)

// microsoftIMAPTokenSource returns an XOAUTH2 token callback for a Microsoft
// 365 IMAP source, selecting between the two auth models:
//
//   - App-only (MSAppOnly): a certificate client-credentials assertion mints a
//     token for the shared mailbox. No user, no refresh token — the
//     unattended-service path. Requires nothing in the [microsoft] config
//     section; all parameters live on the source config.
//   - Delegated (default): a per-user browser-authorized refresh token from the
//     shared [microsoft] app registration.
func microsoftIMAPTokenSource(
	ctx context.Context,
	imapCfg *imapclient.Config,
) (func(context.Context) (string, error), error) {
	if imapCfg.MSAppOnly {
		ts, err := microsoft.NewAppOnlyTokenSource(microsoft.AppOnlyConfig{
			TenantID: imapCfg.MSTenantID,
			ClientID: imapCfg.MSClientID,
			CertPath: imapCfg.MSCertPath,
			Mailbox:  imapCfg.Username,
		}, logger)
		if err != nil {
			return nil, fmt.Errorf("build app-only token source: %w", err)
		}
		return ts.TokenFunc(), nil
	}

	if cfg.Microsoft.ClientID == "" {
		return nil, errors.New(
			"microsoft OAuth not configured — add a [microsoft] section with client_id to config.toml")
	}
	msMgr := microsoft.NewManager(
		cfg.Microsoft.ClientID,
		cfg.Microsoft.EffectiveTenantID(),
		cfg.Microsoft.EffectiveRedirectURI(),
		cfg.TokensDir(),
		logger,
	)
	tokenFn, err := msMgr.TokenSource(ctx, imapCfg.Username)
	if err != nil {
		return nil, fmt.Errorf("load Microsoft token: %w (run 'add-o365' first)", err)
	}
	return tokenFn, nil
}

package microsoft

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // SHA-1 is required by the x5t JWT header (RFC 7515); not used for security
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// appOnlyIMAPScope is the resource scope for certificate-based app-only
// (client-credentials) access to Exchange Online IMAP. Unlike the delegated
// IMAP.AccessAsUser.All scope, app-only tokens request the ".default" scope
// and carry the IMAP.AccessAsApp application permission granted (with admin
// consent) to the Entra app registration.
const appOnlyIMAPScope = "https://outlook.office365.com/.default"

// appOnlyTokenSkew is subtracted from a token's reported lifetime so a fresh
// token is fetched slightly before the current one actually expires.
const appOnlyTokenSkew = 2 * time.Minute

// appOnlyAssertionLifetime bounds the signed client-assertion JWT. Microsoft
// requires exp within a few minutes of iat; 10 minutes is comfortably inside
// the accepted window.
const appOnlyAssertionLifetime = 10 * time.Minute

// AppOnlyConfig holds the parameters for certificate-based app-only auth
// against Exchange Online IMAP. This is the unattended-service path: no user,
// no refresh token, no interactive consent — a signed JWT client assertion is
// exchanged for an access token scoped to a single shared mailbox.
type AppOnlyConfig struct {
	// TenantID is the Entra (Azure AD) tenant GUID or verified domain.
	TenantID string
	// ClientID is the Entra app registration's application (client) ID.
	ClientID string
	// CertPath is a PEM file containing BOTH the RSA private key and the
	// certificate whose public half is registered on the app.
	CertPath string
	// Mailbox is the shared mailbox the app is scoped to; it becomes the
	// XOAUTH2 username. The app's service principal must be granted access
	// to this mailbox via Exchange Online RBAC for Applications.
	Mailbox string
}

// Validate reports whether the config has the fields required to build a
// token source.
func (c AppOnlyConfig) Validate() error {
	var missing []string
	if strings.TrimSpace(c.TenantID) == "" {
		missing = append(missing, "tenant")
	}
	if strings.TrimSpace(c.ClientID) == "" {
		missing = append(missing, "client-id")
	}
	if strings.TrimSpace(c.CertPath) == "" {
		missing = append(missing, "cert")
	}
	if strings.TrimSpace(c.Mailbox) == "" {
		missing = append(missing, "mailbox")
	}
	if len(missing) > 0 {
		return fmt.Errorf("app-only config missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// AppOnlyTokenSource mints and caches app-only access tokens for one mailbox.
// It is safe for concurrent use.
type AppOnlyTokenSource struct {
	cfg    AppOnlyConfig
	key    *rsa.PrivateKey
	x5t    string
	logger *slog.Logger

	httpc *http.Client
	// now and newJTI are injectable for deterministic tests.
	now    func() time.Time
	newJTI func() (string, error)
	// endpointOverride, when set, replaces the live AAD token endpoint. Tests
	// only; empty means the real tenant endpoint is used.
	endpointOverride string

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewAppOnlyTokenSource loads the certificate/key from cfg.CertPath, computes
// the x5t thumbprint used in the assertion header, and returns a ready token
// source. It does not contact the network.
func NewAppOnlyTokenSource(cfg AppOnlyConfig, logger *slog.Logger) (*AppOnlyTokenSource, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	key, x5t, err := loadAppOnlyCert(cfg.CertPath)
	if err != nil {
		return nil, err
	}
	return &AppOnlyTokenSource{
		cfg:    cfg,
		key:    key,
		x5t:    x5t,
		logger: logger,
		httpc:  &http.Client{Timeout: tokenRefreshTimeout},
		now:    time.Now,
		newJTI: newJTI,
	}, nil
}

// TokenFunc adapts the source to the callback shape expected by
// imap.WithTokenSource.
func (s *AppOnlyTokenSource) TokenFunc() func(context.Context) (string, error) {
	return s.Token
}

// Token returns a valid access token, minting a new one when the cached token
// is missing or within appOnlyTokenSkew of expiry.
func (s *AppOnlyTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && s.now().Before(s.expiresAt.Add(-appOnlyTokenSkew)) {
		return s.token, nil
	}
	tok, exp, err := s.fetch(ctx)
	if err != nil {
		return "", err
	}
	s.token = tok
	s.expiresAt = exp
	return tok, nil
}

// tokenEndpoint returns the v2.0 token URL for the configured tenant.
func (s *AppOnlyTokenSource) tokenEndpoint() string {
	if s.endpointOverride != "" {
		return s.endpointOverride
	}
	return fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token",
		url.PathEscape(s.cfg.TenantID))
}

// clientAssertion builds and RS256-signs the JWT client assertion proving
// possession of the certificate's private key.
func (s *AppOnlyTokenSource) clientAssertion(now time.Time) (string, error) {
	jti, err := s.newJTI()
	if err != nil {
		return "", fmt.Errorf("generate assertion jti: %w", err)
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT", "x5t": s.x5t}
	claims := map[string]any{
		"aud": s.tokenEndpoint(),
		"iss": s.cfg.ClientID,
		"sub": s.cfg.ClientID,
		"jti": jti,
		"nbf": now.Unix(),
		"iat": now.Unix(),
		"exp": now.Add(appOnlyAssertionLifetime).Unix(),
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign client assertion: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// fetch performs the client-credentials token request and returns the access
// token and its absolute expiry.
func (s *AppOnlyTokenSource) fetch(ctx context.Context) (string, time.Time, error) {
	now := s.now()
	assertion, err := s.clientAssertion(now)
	if err != nil {
		return "", time.Time{}, err
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {s.cfg.ClientID},
		"scope":                 {appOnlyIMAPScope},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenEndpoint(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpc.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, appOnlyTokenError(resp.StatusCode, body)
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, errors.New("token response had empty access_token")
	}
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour // Exchange app tokens are ~60–90 min; be conservative.
	}
	s.logger.Debug("minted app-only IMAP token",
		"mailbox", s.cfg.Mailbox, "expires_in_s", tr.ExpiresIn)
	return tr.AccessToken, now.Add(lifetime), nil
}

// appOnlyTokenError extracts the AAD error/description for a non-200 response.
func appOnlyTokenError(status int, body []byte) error {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		desc := e.Description
		if i := strings.IndexByte(desc, '\n'); i > 0 {
			desc = desc[:i] // first line only; AAD descriptions are multi-line
		}
		return fmt.Errorf("app-only token request failed (%d): %s: %s", status, e.Error, desc)
	}
	return fmt.Errorf("app-only token request failed (%d)", status)
}

// loadAppOnlyCert parses a PEM file containing an RSA private key and a
// certificate, returning the key and the base64url SHA-1 thumbprint (x5t)
// of the certificate.
func loadAppOnlyCert(path string) (*rsa.PrivateKey, string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied cert path on the local machine
	if err != nil {
		return nil, "", fmt.Errorf("read cert file %s: %w", path, err)
	}
	var key *rsa.PrivateKey
	var certDER []byte
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			if certDER == nil {
				certDER = block.Bytes
			}
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, "", fmt.Errorf("parse RSA private key: %w", err)
			}
		case "PRIVATE KEY":
			parsed, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
			if perr != nil {
				return nil, "", fmt.Errorf("parse PKCS#8 private key: %w", perr)
			}
			rsaKey, ok := parsed.(*rsa.PrivateKey)
			if !ok {
				return nil, "", fmt.Errorf("private key is %T, want RSA", parsed)
			}
			key = rsaKey
		}
	}
	if key == nil {
		return nil, "", fmt.Errorf("no RSA private key found in %s", path)
	}
	if certDER == nil {
		return nil, "", fmt.Errorf("no certificate found in %s (needed for x5t thumbprint)", path)
	}
	sum := sha1.Sum(certDER) //nolint:gosec // x5t is defined as the SHA-1 thumbprint
	return key, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// newJTI returns a random RFC 4122-ish unique identifier for the assertion.
func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

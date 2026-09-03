// Package remoteoauth provides a small OAuth 2.1 authorization server for a
// single-user remote MCP endpoint. It deliberately supports only preregistered
// confidential clients, authorization-code + PKCE, and rotating refresh tokens.
package remoteoauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultAccessTTL  = time.Hour
	defaultRefreshTTL = 30 * 24 * time.Hour
	transactionTTL    = 10 * time.Minute
	authorizationTTL  = 5 * time.Minute
	maxLoginAttempts  = 5
	maxFormBytes      = 64 << 10
)

var errInvalidGrant = errors.New("invalid grant")

// Config describes one protected MCP resource and its preregistered client.
type Config struct {
	Issuer          string
	Resource        string
	ClientID        string
	ClientSecret    string
	LoginUser       string
	LoginPassword   string
	StateFile       string
	RedirectURIs    []string
	Scopes          []string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	Now             func() time.Time
}

// Server serves OAuth metadata and endpoints and protects an MCP handler.
type Server struct {
	cfg          Config
	redirectURIs map[string]bool
	store        *tokenStore
	mu           sync.Mutex
	transactions map[string]authorizationRequest
	codes        map[string]authorizationCode
}

type authorizationRequest struct {
	ExpiresAt     time.Time
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
	Scope         string
	Resource      string
	Attempts      int
}

type authorizationCode struct {
	ExpiresAt     time.Time
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Scope         string
	Resource      string
	Subject       string
}

type tokenRecord struct {
	ExpiresAt time.Time `json:"expires_at"`
	ClientID  string    `json:"client_id"`
	Subject   string    `json:"subject"`
	Scope     string    `json:"scope"`
	Resource  string    `json:"resource"`
}

type persistedTokens struct {
	Version int                    `json:"version"`
	Access  map[string]tokenRecord `json:"access"`
	Refresh map[string]tokenRecord `json:"refresh"`
}

type tokenStore struct {
	mu      sync.Mutex
	path    string
	now     func() time.Time
	access  map[string]tokenRecord
	refresh map[string]tokenRecord
}

// New validates cfg and loads persistent token state.
func New(cfg Config) (*Server, error) {
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	cfg.Resource = strings.TrimSpace(cfg.Resource)
	if cfg.Resource == "" {
		cfg.Resource = cfg.Issuer + "/mcp"
	}
	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = defaultAccessTTL
	}
	if cfg.RefreshTokenTTL <= 0 {
		cfg.RefreshTokenTTL = defaultRefreshTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"whatsapp:bridge"}
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	store, err := loadTokenStore(cfg.StateFile, cfg.Now)
	if err != nil {
		return nil, err
	}
	redirects := make(map[string]bool, len(cfg.RedirectURIs))
	for _, raw := range cfg.RedirectURIs {
		redirects[strings.TrimSpace(raw)] = true
	}
	return &Server{
		cfg:          cfg,
		redirectURIs: redirects,
		store:        store,
		transactions: make(map[string]authorizationRequest),
		codes:        make(map[string]authorizationCode),
	}, nil
}

func validateConfig(cfg Config) error {
	issuer, err := url.Parse(cfg.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("remote OAuth issuer must be an HTTPS origin")
	}
	resource, err := url.Parse(cfg.Resource)
	if err != nil || resource.Scheme != "https" || resource.Host == "" {
		return errors.New("remote OAuth resource must be an HTTPS URL")
	}
	if cfg.ClientID == "" {
		return errors.New("remote OAuth client id is required")
	}
	if len(cfg.ClientSecret) < 32 {
		return errors.New("remote OAuth client secret must be at least 32 characters")
	}
	if cfg.LoginUser == "" {
		return errors.New("remote OAuth login user is required")
	}
	if len(cfg.LoginPassword) < 16 {
		return errors.New("remote OAuth login password must be at least 16 characters")
	}
	if cfg.StateFile == "" {
		return errors.New("remote OAuth state file is required")
	}
	if len(cfg.RedirectURIs) == 0 {
		return errors.New("at least one remote OAuth redirect URI is required")
	}
	for _, raw := range cfg.RedirectURIs {
		redirect, parseErr := url.Parse(strings.TrimSpace(raw))
		if parseErr != nil || redirect.Host == "" || redirect.Scheme != "https" || redirect.RawQuery != "" || redirect.Fragment != "" {
			return fmt.Errorf("invalid remote OAuth redirect URI %q", raw)
		}
	}
	for _, scope := range cfg.Scopes {
		if strings.TrimSpace(scope) == "" || strings.ContainsAny(scope, " \t\r\n") {
			return fmt.Errorf("invalid remote OAuth scope %q", scope)
		}
	}
	return nil
}

// RegisterRoutes adds OAuth discovery, authorization, and token endpoints.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleAuthorizationServerMetadata)
	mux.HandleFunc("/authorize", s.handleAuthorize)
	mux.HandleFunc("/oauth/login", s.handleLogin)
	mux.HandleFunc("/token", s.handleToken)
}

// Protect requires a valid access token before forwarding to next.
func (s *Server) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential, ok := bearerCredential(r.Header.Values("Authorization"))
		if !ok || !s.store.validAccess(credential, s.cfg.Resource) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer resource_metadata=%q, scope=%q`,
				s.cfg.Issuer+"/.well-known/oauth-protected-resource/mcp",
				strings.Join(s.cfg.Scopes, " "),
			))
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerCredential(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	scheme, credential, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" || strings.ContainsAny(credential, " \t\r\n") {
		return "", false
	}
	return credential, true
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, map[string]any{
		"resource":                 s.cfg.Resource,
		"authorization_servers":    []string{s.cfg.Issuer},
		"scopes_supported":         s.cfg.Scopes,
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "WhatsApp bridge",
	})
}

func (s *Server) handleAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, map[string]any{
		"issuer":                                s.cfg.Issuer,
		"authorization_endpoint":                s.cfg.Issuer + "/authorize",
		"token_endpoint":                        s.cfg.Issuer + "/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"scopes_supported":                      s.cfg.Scopes,
	})
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	req, err := s.parseAuthorizationRequest(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tx, err := randomToken("mvt_")
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.cleanupEphemeralLocked()
	s.transactions[tokenHash(tx)] = req
	s.mu.Unlock()
	renderLogin(w, s.cfg.LoginUser, tx, "", http.StatusOK)
}

func (s *Server) parseAuthorizationRequest(values url.Values) (authorizationRequest, error) {
	if values.Get("response_type") != "code" {
		return authorizationRequest{}, errors.New("response_type must be code")
	}
	if values.Get("client_id") != s.cfg.ClientID {
		return authorizationRequest{}, errors.New("unknown client_id")
	}
	redirect := values.Get("redirect_uri")
	if !s.redirectURIs[redirect] {
		return authorizationRequest{}, errors.New("redirect_uri is not registered")
	}
	if values.Get("state") == "" {
		return authorizationRequest{}, errors.New("state is required")
	}
	challenge := values.Get("code_challenge")
	if values.Get("code_challenge_method") != "S256" || !validCodeChallenge(challenge) {
		return authorizationRequest{}, errors.New("S256 PKCE is required")
	}
	scope, err := s.normalizeScope(values.Get("scope"))
	if err != nil {
		return authorizationRequest{}, err
	}
	resource := values.Get("resource")
	if resource == "" {
		resource = s.cfg.Resource
	}
	if resource != s.cfg.Resource {
		return authorizationRequest{}, errors.New("invalid resource")
	}
	return authorizationRequest{
		ExpiresAt:     s.cfg.Now().Add(transactionTTL),
		ClientID:      s.cfg.ClientID,
		RedirectURI:   redirect,
		State:         values.Get("state"),
		CodeChallenge: challenge,
		Scope:         scope,
		Resource:      resource,
	}, nil
}

func validCodeChallenge(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func (s *Server) normalizeScope(raw string) (string, error) {
	requested := strings.Fields(raw)
	if len(requested) == 0 {
		requested = append([]string(nil), s.cfg.Scopes...)
	}
	seen := make(map[string]bool, len(requested))
	for _, scope := range requested {
		if !slices.Contains(s.cfg.Scopes, scope) {
			return "", fmt.Errorf("unsupported scope %q", scope)
		}
		seen[scope] = true
	}
	normalized := make([]string, 0, len(seen))
	for _, scope := range s.cfg.Scopes {
		if seen[scope] {
			normalized = append(normalized, scope)
		}
	}
	return strings.Join(normalized, " "), nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	tx := strings.TrimSpace(r.Form.Get("transaction"))
	hash := tokenHash(tx)
	s.mu.Lock()
	s.cleanupEphemeralLocked()
	req, ok := s.transactions[hash]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "authorization request expired", http.StatusBadRequest)
		return
	}
	if !constantEqual(r.Form.Get("password"), s.cfg.LoginPassword) {
		req.Attempts++
		if req.Attempts >= maxLoginAttempts {
			delete(s.transactions, hash)
		} else {
			s.transactions[hash] = req
		}
		s.mu.Unlock()
		renderLogin(w, s.cfg.LoginUser, tx, "Invalid credentials", http.StatusUnauthorized)
		return
	}
	delete(s.transactions, hash)
	code, err := randomToken("mvc_")
	if err != nil {
		s.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	s.codes[tokenHash(code)] = authorizationCode{
		ExpiresAt:     s.cfg.Now().Add(authorizationTTL),
		ClientID:      req.ClientID,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		Scope:         req.Scope,
		Resource:      req.Resource,
		Subject:       s.cfg.LoginUser,
	}
	s.mu.Unlock()
	redirect, _ := url.Parse(req.RedirectURI)
	query := redirect.Query()
	query.Set("code", code)
	query.Set("state", req.State)
	redirect.RawQuery = query.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (s *Server) cleanupEphemeralLocked() {
	now := s.cfg.Now()
	for key, tx := range s.transactions {
		if !tx.ExpiresAt.After(now) {
			delete(s.transactions, key)
		}
	}
	for key, code := range s.codes {
		if !code.ExpiresAt.After(now) {
			delete(s.codes, key)
		}
	}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, "invalid_request", "invalid form", http.StatusBadRequest)
		return
	}
	clientID, clientSecret, ok := tokenClientCredentials(r)
	if !ok || clientID != s.cfg.ClientID || !constantEqual(clientSecret, s.cfg.ClientSecret) {
		w.Header().Set("WWW-Authenticate", "Basic")
		writeOAuthError(w, "invalid_client", "client authentication failed", http.StatusUnauthorized)
		return
	}
	var response map[string]any
	var err error
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		response, err = s.exchangeAuthorizationCode(r.Form)
	case "refresh_token":
		response, err = s.exchangeRefreshToken(r.Form)
	default:
		writeOAuthError(w, "unsupported_grant_type", "unsupported grant_type", http.StatusBadRequest)
		return
	}
	if err != nil {
		if errors.Is(err, errInvalidGrant) {
			writeOAuthError(w, "invalid_grant", "grant is invalid or expired", http.StatusBadRequest)
		} else {
			writeOAuthError(w, "server_error", "token state could not be saved", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, response)
}

func tokenClientCredentials(r *http.Request) (string, string, bool) {
	formID, formSecret := r.Form.Get("client_id"), r.Form.Get("client_secret")
	basicID, basicSecret, basicOK := r.BasicAuth()
	if basicOK {
		if (formID != "" && formID != basicID) || (formSecret != "" && formSecret != basicSecret) {
			return "", "", false
		}
		return basicID, basicSecret, true
	}
	return formID, formSecret, formID != "" && formSecret != ""
}

func (s *Server) exchangeAuthorizationCode(form url.Values) (map[string]any, error) {
	codeValue := form.Get("code")
	s.mu.Lock()
	s.cleanupEphemeralLocked()
	code, ok := s.codes[tokenHash(codeValue)]
	delete(s.codes, tokenHash(codeValue))
	s.mu.Unlock()
	if !ok || code.ClientID != s.cfg.ClientID || code.RedirectURI != form.Get("redirect_uri") ||
		code.Resource != s.cfg.Resource || !verifyPKCE(code.CodeChallenge, form.Get("code_verifier")) {
		return nil, errInvalidGrant
	}
	return s.issueTokenResponse(code.Subject, code.Scope, code.Resource)
}

func (s *Server) exchangeRefreshToken(form url.Values) (map[string]any, error) {
	resource := form.Get("resource")
	if resource == "" {
		resource = s.cfg.Resource
	}
	requestedScope := form.Get("scope")
	return s.store.rotateRefresh(form.Get("refresh_token"), s.cfg.ClientID, resource, requestedScope,
		s.cfg.AccessTokenTTL, s.cfg.RefreshTokenTTL)
}

func (s *Server) issueTokenResponse(subject, scope, resource string) (map[string]any, error) {
	return s.store.issue(subject, s.cfg.ClientID, scope, resource, s.cfg.AccessTokenTTL, s.cfg.RefreshTokenTTL)
}

func verifyPKCE(challenge, verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return constantEqual(challenge, got)
}

func constantEqual(a, b string) bool {
	aHash := sha256.Sum256([]byte(a))
	bHash := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aHash[:], bHash[:]) == 1
}

func randomToken(prefix string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func loadTokenStore(path string, now func() time.Time) (*tokenStore, error) {
	store := &tokenStore{
		path:    path,
		now:     now,
		access:  make(map[string]tokenRecord),
		refresh: make(map[string]tokenRecord),
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read remote OAuth state: %w", err)
	}
	var persisted persistedTokens
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.Version != 1 {
		return nil, errors.New("invalid remote OAuth state file")
	}
	if persisted.Access != nil {
		store.access = persisted.Access
	}
	if persisted.Refresh != nil {
		store.refresh = persisted.Refresh
	}
	store.cleanupLocked()
	return store, nil
}

func (s *tokenStore) issue(subject, clientID, scope, resource string, accessTTL, refreshTTL time.Duration) (map[string]any, error) {
	access, err := randomToken("mva_")
	if err != nil {
		return nil, err
	}
	refresh, err := randomToken("mvr_")
	if err != nil {
		return nil, err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
	record := tokenRecord{ClientID: clientID, Subject: subject, Scope: scope, Resource: resource}
	accessRecord := record
	accessRecord.ExpiresAt = now.Add(accessTTL)
	refreshRecord := record
	refreshRecord.ExpiresAt = now.Add(refreshTTL)
	s.access[tokenHash(access)] = accessRecord
	s.refresh[tokenHash(refresh)] = refreshRecord
	if err := s.saveLocked(); err != nil {
		delete(s.access, tokenHash(access))
		delete(s.refresh, tokenHash(refresh))
		return nil, err
	}
	return tokenResponse(access, refresh, scope, accessTTL), nil
}

func (s *tokenStore) rotateRefresh(value, clientID, resource, requestedScope string, accessTTL, refreshTTL time.Duration) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
	hash := tokenHash(value)
	record, ok := s.refresh[hash]
	if !ok || record.ClientID != clientID || record.Resource != resource {
		return nil, errInvalidGrant
	}
	scope := record.Scope
	if requestedScope != "" {
		requested := strings.Fields(requestedScope)
		granted := strings.Fields(record.Scope)
		for _, item := range requested {
			if !slices.Contains(granted, item) {
				return nil, errInvalidGrant
			}
		}
		scope = strings.Join(requested, " ")
	}
	delete(s.refresh, hash)
	access, err := randomToken("mva_")
	if err != nil {
		return nil, err
	}
	refresh, err := randomToken("mvr_")
	if err != nil {
		return nil, err
	}
	now := s.now()
	record.Scope = scope
	record.ExpiresAt = now.Add(accessTTL)
	s.access[tokenHash(access)] = record
	record.ExpiresAt = now.Add(refreshTTL)
	s.refresh[tokenHash(refresh)] = record
	if err := s.saveLocked(); err != nil {
		delete(s.access, tokenHash(access))
		delete(s.refresh, tokenHash(refresh))
		s.refresh[hash] = record
		return nil, err
	}
	return tokenResponse(access, refresh, scope, accessTTL), nil
}

func tokenResponse(access, refresh, scope string, accessTTL time.Duration) map[string]any {
	return map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int64(accessTTL / time.Second),
		"refresh_token": refresh,
		"scope":         scope,
	}
}

func (s *tokenStore) validAccess(value, resource string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.access[tokenHash(value)]
	return ok && record.Resource == resource && record.ExpiresAt.After(s.now())
}

func (s *tokenStore) cleanupLocked() {
	now := s.now()
	for hash, record := range s.access {
		if !record.ExpiresAt.After(now) {
			delete(s.access, hash)
		}
	}
	for hash, record := range s.refresh {
		if !record.ExpiresAt.After(now) {
			delete(s.refresh, hash)
		}
	}
}

func (s *tokenStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create remote OAuth state directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".remote-oauth-*")
	if err != nil {
		return fmt.Errorf("create remote OAuth state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	if err := encoder.Encode(persistedTokens{Version: 1, Access: s.access, Refresh: s.refresh}); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode remote OAuth state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync remote OAuth state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close remote OAuth state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace remote OAuth state: %w", err)
	}
	return nil
}

func methodNotAllowed(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func writeOAuthError(w http.ResponseWriter, code, description string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize WhatsApp bridge</title></head><body>
<main><h1>Authorize WhatsApp bridge</h1><p>Sign in as {{.User}} to connect this bridge to Claude.</p>
{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}
<form method="post" action="/oauth/login"><input type="hidden" name="transaction" value="{{.Transaction}}">
<label>Password <input name="password" type="password" autocomplete="current-password" required autofocus></label>
<button type="submit">Authorize</button></form></main></body></html>`))

func renderLogin(w http.ResponseWriter, user, transaction, message string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = loginTemplate.Execute(w, map[string]string{"User": user, "Transaction": transaction, "Error": message})
}

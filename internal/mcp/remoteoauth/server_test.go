package remoteoauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testIssuer       = "https://whatsapp.example.test"
	testResource     = testIssuer + "/mcp"
	testClientID     = "claude-work-whatsapp"
	testClientSecret = "0123456789abcdef0123456789abcdef"
	testLoginUser    = "operator@example.test"
	testLoginPass    = "correct horse battery staple"
	testRedirect     = "https://claude.ai/api/mcp/auth_callback"
	testVerifier     = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

func newTestServer(t *testing.T, stateFile string, now *time.Time) *Server {
	t.Helper()
	server, err := New(Config{
		Issuer:        testIssuer,
		ClientID:      testClientID,
		ClientSecret:  testClientSecret,
		LoginUser:     testLoginUser,
		LoginPassword: testLoginPass,
		StateFile:     stateFile,
		RedirectURIs:  []string{testRedirect},
		Now:           func() time.Time { return *now },
	})
	require.NoError(t, err)
	return server
}

func testHandler(server *Server) http.Handler {
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	mux.Handle("/mcp", server.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	return mux
}

func TestAuthorizationCodePKCEAndPersistentTokens(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	stateFile := filepath.Join(t.TempDir(), "oauth-state.json")
	server := newTestServer(t, stateFile, &now)
	handler := testHandler(server)

	challengeSum := sha256.Sum256([]byte(testVerifier))
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {testClientID},
		"redirect_uri":          {testRedirect},
		"state":                 {"claude-state"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challengeSum[:])},
		"code_challenge_method": {"S256"},
		"resource":              {testResource},
	}
	authorize := httptest.NewRecorder()
	handler.ServeHTTP(authorize, httptest.NewRequest(http.MethodGet, "/authorize?"+query.Encode(), nil))
	require.Equal(t, http.StatusOK, authorize.Code)
	transaction := regexp.MustCompile(`name="transaction" value="([^"]+)"`).FindStringSubmatch(authorize.Body.String())
	require.Len(t, transaction, 2)

	badLogin := httptest.NewRecorder()
	badForm := url.Values{"transaction": {transaction[1]}, "password": {"wrong"}}
	handler.ServeHTTP(badLogin, formRequest(http.MethodPost, "/oauth/login", badForm))
	assert.Equal(t, http.StatusUnauthorized, badLogin.Code)
	assert.Contains(t, badLogin.Body.String(), "Invalid credentials")

	login := httptest.NewRecorder()
	loginForm := url.Values{"transaction": {transaction[1]}, "password": {testLoginPass}}
	handler.ServeHTTP(login, formRequest(http.MethodPost, "/oauth/login", loginForm))
	require.Equal(t, http.StatusFound, login.Code)
	callback, err := url.Parse(login.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "claude-state", callback.Query().Get("state"))
	require.NotEmpty(t, callback.Query().Get("code"))

	token := httptest.NewRecorder()
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {callback.Query().Get("code")},
		"redirect_uri":  {testRedirect},
		"code_verifier": {testVerifier},
	}
	tokenRequest := formRequest(http.MethodPost, "/token", tokenForm)
	tokenRequest.SetBasicAuth(testClientID, testClientSecret)
	handler.ServeHTTP(token, tokenRequest)
	require.Equal(t, http.StatusOK, token.Code, token.Body.String())
	issued := decodeTokenResponse(t, token.Body)
	assert.Equal(t, "Bearer", issued.TokenType)
	require.NotEmpty(t, issued.AccessToken)
	require.NotEmpty(t, issued.RefreshToken)

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	assert.Equal(t, http.StatusUnauthorized, missing.Code)
	assert.Contains(t, missing.Header().Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp")

	authorized := httptest.NewRecorder()
	authorizedRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer "+issued.AccessToken)
	handler.ServeHTTP(authorized, authorizedRequest)
	assert.Equal(t, http.StatusNoContent, authorized.Code)

	// Access survives a process restart because only token hashes are persisted.
	reloaded := testHandler(newTestServer(t, stateFile, &now))
	afterRestart := httptest.NewRecorder()
	afterRestartRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	afterRestartRequest.Header.Set("Authorization", "Bearer "+issued.AccessToken)
	reloaded.ServeHTTP(afterRestart, afterRestartRequest)
	assert.Equal(t, http.StatusNoContent, afterRestart.Code)
	stateBytes, err := io.ReadAll(requireOpen(t, stateFile))
	require.NoError(t, err)
	assert.NotContains(t, string(stateBytes), issued.AccessToken)
	assert.NotContains(t, string(stateBytes), issued.RefreshToken)

	refresh := httptest.NewRecorder()
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {issued.RefreshToken}}
	refreshRequest := formRequest(http.MethodPost, "/token", refreshForm)
	refreshRequest.SetBasicAuth(testClientID, testClientSecret)
	reloaded.ServeHTTP(refresh, refreshRequest)
	require.Equal(t, http.StatusOK, refresh.Code, refresh.Body.String())
	rotated := decodeTokenResponse(t, refresh.Body)
	assert.NotEqual(t, issued.RefreshToken, rotated.RefreshToken)

	reuse := httptest.NewRecorder()
	reuseRequest := formRequest(http.MethodPost, "/token", refreshForm)
	reuseRequest.SetBasicAuth(testClientID, testClientSecret)
	reloaded.ServeHTTP(reuse, reuseRequest)
	assert.Equal(t, http.StatusBadRequest, reuse.Code)
}

func TestMetadataAndAuthorizationValidation(t *testing.T) {
	now := time.Now()
	server := newTestServer(t, filepath.Join(t.TempDir(), "state.json"), &now)
	handler := testHandler(server)

	metadata := httptest.NewRecorder()
	handler.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	require.Equal(t, http.StatusOK, metadata.Code)
	assert.Contains(t, metadata.Body.String(), `"code_challenge_methods_supported":["S256"]`)
	assert.Contains(t, metadata.Body.String(), testIssuer+"/token")

	protectedMetadata := httptest.NewRecorder()
	handler.ServeHTTP(protectedMetadata, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	require.Equal(t, http.StatusOK, protectedMetadata.Code)
	assert.Contains(t, protectedMetadata.Body.String(), testResource)

	for name, values := range map[string]url.Values{
		"unknown client": {
			"response_type": {"code"}, "client_id": {"other"}, "redirect_uri": {testRedirect},
		},
		"open redirect": {
			"response_type": {"code"}, "client_id": {testClientID}, "redirect_uri": {"https://evil.example/callback"},
		},
		"plain PKCE": {
			"response_type": {"code"}, "client_id": {testClientID}, "redirect_uri": {testRedirect},
			"state": {"x"}, "code_challenge": {"x"}, "code_challenge_method": {"plain"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/authorize?"+values.Encode(), nil))
			assert.Equal(t, http.StatusBadRequest, response.Code)
		})
	}
}

func formRequest(method, target string, values url.Values) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

type testTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
}

func decodeTokenResponse(t *testing.T, body io.Reader) testTokenResponse {
	t.Helper()
	var response testTokenResponse
	require.NoError(t, json.NewDecoder(body).Decode(&response))
	return response
}

func requireOpen(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	return file
}

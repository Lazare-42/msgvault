package microsoft

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // verifying the x5t thumbprint, which is defined as SHA-1
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestCertPEM generates a self-signed RSA cert + key and writes both to a
// single PEM file, returning the path and the parsed key.
func writeTestCertPEM(t *testing.T, pkcs8 bool) (string, *rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "msgvault-app-only-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	var keyBlock *pem.Block
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		keyBlock = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		keyBlock = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	}

	path := filepath.Join(t.TempDir(), "cert.pem")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, pem.Encode(f, keyBlock))
	require.NoError(t, pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	require.NoError(t, f.Close())
	return path, key, certDER
}

func sha1Sum(b []byte) []byte {
	s := sha1.Sum(b) //nolint:gosec // x5t thumbprint is SHA-1 by spec
	return s[:]
}

func testConfig(certPath string) AppOnlyConfig {
	return AppOnlyConfig{
		TenantID: "00000000-0000-0000-0000-000000000000",
		ClientID: "11111111-1111-1111-1111-111111111111",
		CertPath: certPath,
		Mailbox:  "hr@example.com",
	}
}

func TestAppOnlyConfigValidate(t *testing.T) {
	err := AppOnlyConfig{}.Validate()
	require.Error(t, err)
	for _, want := range []string{"tenant", "client-id", "cert", "mailbox"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NoError(t, testConfig("/x").Validate())
}

func TestLoadAppOnlyCertBothKeyFormats(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		path, key, certDER := writeTestCertPEM(t, pkcs8)
		gotKey, x5t, err := loadAppOnlyCert(path)
		require.NoError(t, err)
		assert.Equal(t, key.N, gotKey.N, "loaded modulus should match")

		// x5t must be the base64url (unpadded) SHA-1 thumbprint of the cert DER.
		want := base64.RawURLEncoding.EncodeToString(sha1Sum(certDER))
		assert.Equal(t, want, x5t)
		assert.NotContains(t, x5t, "=")
	}
}

func TestLoadAppOnlyCertErrors(t *testing.T) {
	_, _, err := loadAppOnlyCert(filepath.Join(t.TempDir(), "nope.pem"))
	require.Error(t, err)

	// cert without a key
	_, _, certDER := writeTestCertPEM(t, false)
	onlyCert := filepath.Join(t.TempDir(), "certonly.pem")
	buf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	require.NoError(t, os.WriteFile(onlyCert, buf, 0o600))
	_, _, err = loadAppOnlyCert(onlyCert)
	require.ErrorContains(t, err, "no RSA private key")
}

func TestClientAssertionIsVerifiableJWT(t *testing.T) {
	path, key, _ := writeTestCertPEM(t, false)
	ts, err := NewAppOnlyTokenSource(testConfig(path), nil)
	require.NoError(t, err)

	now := time.Unix(1_700_000_000, 0)
	assertion, err := ts.clientAssertion(now)
	require.NoError(t, err)

	parts := strings.Split(assertion, ".")
	require.Len(t, parts, 3)

	// header carries the x5t thumbprint and RS256 alg
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	var hdr map[string]string
	require.NoError(t, json.Unmarshal(hdrJSON, &hdr))
	assert.Equal(t, "RS256", hdr["alg"])
	assert.Equal(t, ts.x5t, hdr["x5t"])

	// claims bind issuer/subject to the client id and target the token endpoint
	clJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var cl map[string]any
	require.NoError(t, json.Unmarshal(clJSON, &cl))
	assert.Equal(t, ts.cfg.ClientID, cl["iss"])
	assert.Equal(t, ts.cfg.ClientID, cl["sub"])
	assert.Equal(t, ts.tokenEndpoint(), cl["aud"])
	assert.EqualValues(t, now.Unix(), cl["iat"])
	assert.EqualValues(t, now.Add(appOnlyAssertionLifetime).Unix(), cl["exp"])

	// signature verifies against the cert's public key
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(signingInput))
	require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig))
}

func TestTokenFetchAndCache(t *testing.T) {
	path, _, _ := writeTestCertPEM(t, false)
	ts, err := NewAppOnlyTokenSource(testConfig(path), nil)
	require.NoError(t, err)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "client_credentials", r.Form.Get("grant_type"))
		assert.Equal(t, appOnlyIMAPScope, r.Form.Get("scope"))
		assert.Equal(t,
			"urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
			r.Form.Get("client_assertion_type"))
		assert.NotEmpty(t, r.Form.Get("client_assertion"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	ts.endpointOverride = srv.URL

	// fixed clock so cache logic is deterministic
	base := time.Unix(1_700_000_000, 0)
	ts.now = func() time.Time { return base }

	tok, err := ts.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "tok-abc", tok)
	assert.Equal(t, 1, calls)

	// second call within lifetime is served from cache
	tok2, err := ts.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "tok-abc", tok2)
	assert.Equal(t, 1, calls, "cached token should not trigger a second fetch")

	// advance past expiry (minus skew) → refetch
	ts.now = func() time.Time { return base.Add(59 * time.Minute) }
	_, err = ts.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestTokenFetchAADError(t *testing.T) {
	path, _, _ := writeTestCertPEM(t, false)
	ts, err := NewAppOnlyTokenSource(testConfig(path), nil)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS700027: cert not found\r\nTrace ID: x"}`))
	}))
	defer srv.Close()
	ts.endpointOverride = srv.URL

	_, err = ts.Token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_client")
	assert.Contains(t, err.Error(), "AADSTS700027")
	assert.NotContains(t, err.Error(), "Trace ID", "only the first line of the AAD description is kept")
}

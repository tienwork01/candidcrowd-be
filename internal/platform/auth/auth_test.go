package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func TestGetIdentity(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	_, err := Get(c)
	require.Error(t, err)
	want := Identity{BetterAuthUserID: "better-auth-id", Email: "host@example.com"}
	c.Set(IdentityKey, want)
	got, err := Get(c)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestVerifierMiddleware(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	const keyID = "test-key"
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "OKP",
				"crv": "Ed25519",
				"alg": "EdDSA",
				"use": "sig",
				"kid": keyID,
				"x":   base64.RawURLEncoding.EncodeToString(publicKey),
			}},
		}); encodeErr != nil {
			http.Error(w, encodeErr.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(jwks.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := New(ctx, jwks.URL, "https://auth.example.test", "candidcrowd-api")
	require.NoError(t, err)

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{name: "valid", token: signedToken(t, privateKey, keyID, "https://auth.example.test", "candidcrowd-api", time.Now().Add(time.Minute)), wantStatus: http.StatusNoContent},
		{name: "expired", token: signedToken(t, privateKey, keyID, "https://auth.example.test", "candidcrowd-api", time.Now().Add(-time.Minute)), wantStatus: http.StatusUnauthorized},
		{name: "wrong audience", token: signedToken(t, privateKey, keyID, "https://auth.example.test", "another-api", time.Now().Add(time.Minute)), wantStatus: http.StatusUnauthorized},
		{name: "wrong issuer", token: signedToken(t, privateKey, keyID, "https://other.example.test", "candidcrowd-api", time.Now().Add(time.Minute)), wantStatus: http.StatusUnauthorized},
		{name: "missing bearer", wantStatus: http.StatusUnauthorized},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/protected", verifier.Middleware(), func(c *gin.Context) {
				identity, getErr := Get(c)
				require.NoError(t, getErr)
				require.Equal(t, "better-auth-user", identity.BetterAuthUserID)
				require.True(t, identity.EmailVerified)
				c.Status(http.StatusNoContent)
			})

			request := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, test.wantStatus, response.Code)
		})
	}
}

func signedToken(t *testing.T, key ed25519.PrivateKey, keyID, issuer, audience string, expiresAt time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":             "better-auth-user",
		"iss":             issuer,
		"aud":             audience,
		"iat":             time.Now().Add(-time.Minute).Unix(),
		"exp":             expiresAt.Unix(),
		"email":           "host@example.com",
		"name":            "Host",
		"email_verified":  true,
		"terms_version":   "2026-01",
		"privacy_version": "2026-01",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = keyID
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

func TestVerifierKeyRotation(t *testing.T) {
	pubKey1, privKey1, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubKey2, privKey2, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	var mu sync.RWMutex
	keys := []map[string]any{{
		"kty": "OKP",
		"crv": "Ed25519",
		"alg": "EdDSA",
		"use": "sig",
		"kid": "kid-1",
		"x":   base64.RawURLEncoding.EncodeToString(pubKey1),
	}}

	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(jwks.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := New(ctx, jwks.URL, "https://auth.example.test", "candidcrowd-api")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/protected", verifier.Middleware(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Token with initial key succeeds
	token1 := signedToken(t, privKey1, "kid-1", "https://auth.example.test", "candidcrowd-api", time.Now().Add(time.Minute))
	req1 := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req1.Header.Set("Authorization", "Bearer "+token1)
	res1 := httptest.NewRecorder()
	router.ServeHTTP(res1, req1)
	require.Equal(t, http.StatusOK, res1.Code)

	// Rotate keys on JWKS server: add key 2
	mu.Lock()
	keys = append(keys, map[string]any{
		"kty": "OKP",
		"crv": "Ed25519",
		"alg": "EdDSA",
		"use": "sig",
		"kid": "kid-2",
		"x":   base64.RawURLEncoding.EncodeToString(pubKey2),
	})
	mu.Unlock()

	// Token with rotated key kid-2 triggers refresh and succeeds
	token2 := signedToken(t, privKey2, "kid-2", "https://auth.example.test", "candidcrowd-api", time.Now().Add(time.Minute))
	req2 := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req2.Header.Set("Authorization", "Bearer "+token2)
	res2 := httptest.NewRecorder()
	router.ServeHTTP(res2, req2)
	require.Equal(t, http.StatusOK, res2.Code)
}

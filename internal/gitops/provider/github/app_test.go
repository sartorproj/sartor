/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sartorproj/sartor/internal/gitops/provider"
)

func testKey(t *testing.T, pkcs8 bool) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func verifyJWT(t *testing.T, jwt string, pub *rsa.PublicKey) map[string]interface{} {
	t.Helper()
	parts := strings.Split(jwt, ".")
	require.Len(t, parts, 3)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.NoError(t, rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig))
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	claims := map[string]interface{}{}
	require.NoError(t, json.Unmarshal(payload, &claims))
	return claims
}

func TestGitHubAppAuthentication(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		t.Run(map[bool]string{false: "PKCS1", true: "PKCS8"}[pkcs8], func(t *testing.T) {
			key, pemKey := testKey(t, pkcs8)
			var tokenCalls, apiCalls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v3/app/installations/42/access_tokens":
					tokenCalls.Add(1)
					claims := verifyJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey)
					assert.Equal(t, "123", claims["iss"])
					assert.LessOrEqual(t, claims["exp"].(float64)-float64(time.Now().Unix()), 600.0, "GitHub rejects JWTs expiring more than 10 minutes ahead")
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"token":      "ghs_installation",
						"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					})
				case "/api/v3/installation/repositories":
					apiCalls.Add(1)
					assert.Equal(t, "Bearer ghs_installation", r.Header.Get("Authorization"))
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"total_count": 1, "repositories": []interface{}{}})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			p, err := New(Config{AppID: 123, InstallationID: 42, PrivateKey: pemKey, BaseURL: srv.URL})
			require.NoError(t, err)
			require.NoError(t, p.CheckConnection(context.Background()))
			require.NoError(t, p.CheckConnection(context.Background()))
			assert.Equal(t, int32(1), tokenCalls.Load(), "installation token is cached")
			assert.Equal(t, int32(2), apiCalls.Load())
		})
	}
}

func TestConfigValidate(t *testing.T) {
	_, pemKey := testKey(t, false)
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "token", cfg: Config{Token: "x"}},
		{name: "app", cfg: Config{AppID: 1, InstallationID: 2, PrivateKey: pemKey}},
		{name: "nothing", cfg: Config{}, wantErr: "requires a token"},
		{name: "both", cfg: Config{Token: "x", AppID: 1, InstallationID: 2, PrivateKey: pemKey}, wantErr: "not both"},
		{name: "app without key", cfg: Config{AppID: 1, InstallationID: 2}, wantErr: "privateKey"},
		{name: "app without installation", cfg: Config{AppID: 1, PrivateKey: pemKey}, wantErr: "installationId"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestNewRejectsBadPrivateKey(t *testing.T) {
	_, err := New(Config{AppID: 1, InstallationID: 2, PrivateKey: []byte("not a key")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PEM")
}

func TestGetFileContentNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()
	p, err := New(Config{Token: "t", BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = p.GetFileContent(context.Background(), "acme/gitops", "missing.yaml", "main")
	assert.True(t, errors.Is(err, provider.ErrNotFound), "got %v", err)

	exists, err := p.BranchExists(context.Background(), "acme/gitops", "sartor/x")
	require.NoError(t, err)
	assert.False(t, exists)
}

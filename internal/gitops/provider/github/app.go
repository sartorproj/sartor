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
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/go-github/v93/github"
	"golang.org/x/oauth2"
)

// appTokenSource mints GitHub App installation tokens.
type appTokenSource struct {
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	baseURL        string
	httpClient     *http.Client
	now            func() time.Time
}

func newAppTokenSource(cfg Config, httpClient *http.Client) (*appTokenSource, error) {
	key, err := parsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	return &appTokenSource{
		appID:          cfg.AppID,
		installationID: cfg.InstallationID,
		key:            key,
		baseURL:        cfg.BaseURL,
		httpClient:     httpClient,
		now:            time.Now,
	}, nil
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("GitHub App private key is not PEM encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse GitHub App private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("GitHub App private key must be an RSA key")
	}
	return key, nil
}

// appJWT returns a JWT that authenticates as the GitHub App itself.
func (s *appTokenSource) appJWT() (string, error) {
	now := s.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]interface{}{
		// Backdated to tolerate clock drift, as GitHub recommends.
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(s.appID, 10),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign GitHub App JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Token implements oauth2.TokenSource.
func (s *appTokenSource) Token() (*oauth2.Token, error) {
	jwt, err := s.appJWT()
	if err != nil {
		return nil, err
	}
	client, err := newGitHubClient(s.httpClient, s.baseURL, github.WithAuthToken(jwt))
	if err != nil {
		return nil, err
	}
	tok, _, err := client.Apps.CreateInstallationToken(context.Background(), s.installationID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub App installation token: %w", err)
	}
	return &oauth2.Token{
		AccessToken: tok.GetToken(),
		Expiry:      tok.GetExpiresAt().Time,
	}, nil
}

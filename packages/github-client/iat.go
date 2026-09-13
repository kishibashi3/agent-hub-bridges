// Package githubclient provides GitHub App Installation Access Token (IAT)
// management with automatic refresh.
//
// Usage:
//
//	mgr, err := NewIATManagerFromEnv()
//	if err != nil || mgr == nil {
//	    // not configured — use PAT fallback
//	}
//	tok, err := mgr.GetToken(ctx)
//
// Required env vars (all three must be set to enable IAT mode):
//
//	GITHUB_APP_ID              GitHub App ID (numeric string)
//	GITHUB_APP_PRIVATE_KEY     PEM-encoded RSA private key, or an absolute file path
//	GITHUB_APP_INSTALLATION_ID Installation ID for the target repositories
package githubclient

import (
	"bytes"
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
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// IATManager fetches and caches GitHub App Installation Access Tokens.
// The cached token is refreshed automatically when it will expire within 5 minutes.
// Safe for concurrent use.
type IATManager struct {
	appID          string
	privateKey     *rsa.PrivateKey
	installationID string
	httpClient     *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewIATManagerFromEnv creates an IATManager from GITHUB_APP_* environment variables.
// Returns (nil, nil) when the required vars are not all set — callers should fall back
// to PAT-based authentication in that case.
func NewIATManagerFromEnv() (*IATManager, error) {
	appID := os.Getenv("GITHUB_APP_ID")
	privateKey := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	installationID := os.Getenv("GITHUB_APP_INSTALLATION_ID")

	if appID == "" || privateKey == "" || installationID == "" {
		return nil, nil
	}
	return NewIATManager(appID, privateKey, installationID)
}

// NewIATManager creates an IATManager from explicit credentials.
// privateKeyPEM may be the PEM content directly, or an absolute file path starting with "/".
func NewIATManager(appID, privateKeyPEM, installationID string) (*IATManager, error) {
	key, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("githubclient: parse private key: %w", err)
	}
	return &IATManager{
		appID:          appID,
		privateKey:     key,
		installationID: installationID,
		httpClient:     &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// GetToken returns a valid IAT, fetching a new one if the cached token will expire
// within 5 minutes.
func (m *IATManager) GetToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.token != "" && time.Until(m.expires) > 5*time.Minute {
		return m.token, nil
	}

	tok, exp, err := m.fetchToken(ctx)
	if err != nil {
		return "", err
	}
	m.token = tok
	m.expires = exp
	return tok, nil
}

// fetchToken generates a JWT and exchanges it for an IAT via the GitHub REST API.
func (m *IATManager) fetchToken(ctx context.Context) (string, time.Time, error) {
	jwt, err := m.generateJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate JWT: %w", err)
	}

	url := "https://api.github.com/app/installations/" + m.installationID + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "agent-hub-bridges/github-client")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("IAT request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("IAT request failed: HTTP %d: %s",
			resp.StatusCode, truncateBody(body, 300))
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("decode IAT response: %w", err)
	}
	if result.Token == "" {
		return "", time.Time{}, fmt.Errorf("empty token in IAT response")
	}

	exp, err := time.Parse(time.RFC3339, result.ExpiresAt)
	if err != nil {
		// fallback: assume 1h validity
		exp = time.Now().Add(time.Hour)
	}
	return result.Token, exp, nil
}

// generateJWT creates a GitHub App JWT signed with RS256.
// The JWT is valid for 9 minutes (GitHub limit is 10 minutes).
func (m *IATManager) generateJWT() (string, error) {
	now := time.Now()

	headerBytes, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	payloadBytes, _ := json.Marshal(map[string]any{
		"iss": m.appID,
		"iat": now.Add(-30 * time.Second).Unix(), // 30s clock-skew tolerance
		"exp": now.Add(9 * time.Minute).Unix(),   // stay under 10min GitHub limit
	})

	header := base64.RawURLEncoding.EncodeToString(headerBytes)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	unsigned := header + "." + payload

	h := sha256.New()
	h.Write([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.privateKey, crypto.SHA256, h.Sum(nil))
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parsePrivateKey parses an RSA private key from PEM content or a file path.
func parsePrivateKey(input string) (*rsa.PrivateKey, error) {
	var pemData []byte
	if strings.HasPrefix(input, "/") {
		var err error
		pemData, err = os.ReadFile(input)
		if err != nil {
			return nil, fmt.Errorf("read private key file %q: %w", input, err)
		}
	} else {
		pemData = []byte(input)
	}

	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key")
	}

	// Try PKCS#8 first (GitHub generates PKCS#8), then PKCS#1 for compatibility.
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := parsed.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("PKCS#8 key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func truncateBody(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

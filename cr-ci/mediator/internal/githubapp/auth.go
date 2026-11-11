// Package githubapp is a small, dependency-free GitHub REST client that
// authenticates as a GitHub App: it signs App JWTs (RS256) with the App
// private key, exchanges them for installation access tokens, caches those
// tokens in memory until shortly before they expire, and calls the handful of
// Actions endpoints the mediator needs.
//
// No token, JWT or JIT config ever appears in an error message or log line.
package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	// jwtBackdate absorbs clock skew between us and GitHub (GitHub's docs
	// recommend issuing JWTs 60 s in the past).
	jwtBackdate = 60 * time.Second
	// jwtLifetime stays under GitHub's 10-minute maximum.
	jwtLifetime = 9 * time.Minute
	// tokenRefreshMargin refreshes an installation token this long before it
	// expires (tokens last one hour).
	tokenRefreshMargin = 5 * time.Minute
)

// ParsePrivateKey parses a GitHub App private key in PEM form. GitHub issues
// PKCS#1 ("RSA PRIVATE KEY"); PKCS#8 ("PRIVATE KEY") is also accepted.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github app private key: no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("github app private key: invalid PKCS#1 key")
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("github app private key: invalid PKCS#8 key")
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github app private key: not an RSA key")
		}
		return rk, nil
	default:
		return nil, fmt.Errorf("github app private key: unsupported PEM type %q", block.Type)
	}
}

// signAppJWT returns an RS256 JWT identifying the App, valid for about nine
// minutes from now.
func signAppJWT(key *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{
		Iat: now.Add(-jwtBackdate).Unix(),
		Exp: now.Add(jwtLifetime).Unix(),
		Iss: strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("signing github app jwt failed")
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

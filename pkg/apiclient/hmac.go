// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package apiclient provides shared HTTP client utilities for Scion API clients.
package apiclient

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HMAC authentication headers (must match the constants in pkg/hub/brokerauth.go).
const (
	HeaderBrokerID      = "X-Scion-Broker-ID"
	HeaderTimestamp     = "X-Scion-Timestamp"
	HeaderNonce         = "X-Scion-Nonce"
	HeaderSignature     = "X-Scion-Signature"
	HeaderSignedHeaders = "X-Scion-Signed-Headers"
)

// HMACAuth implements HMAC-based authentication for Runtime Brokers.
// The hub and broker verifiers use the same BuildCanonicalString.
type HMACAuth struct {
	BrokerID  string
	SecretKey []byte
}

// ApplyAuth adds HMAC authentication headers to the request.
func (a *HMACAuth) ApplyAuth(req *http.Request) error {
	if a.BrokerID == "" || len(a.SecretKey) == 0 {
		return fmt.Errorf("HMAC auth requires brokerID and secretKey")
	}

	// 1. Generate timestamp (Unix epoch seconds as string)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	// 2. Generate nonce (16 bytes, URL-safe base64)
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("failed to generate nonce: %w", err)
	}
	nonce := base64.URLEncoding.EncodeToString(nonceBytes)

	// 3. Set headers before building canonical string
	req.Header.Set(HeaderBrokerID, a.BrokerID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)

	// 4. Give the request a sized body, so the body is sent with a
	// Content-Length and covered by the signature.
	if err := sizeRequestBody(req); err != nil {
		return err
	}

	// 5. Build canonical string. A body read error fails the signing:
	// the signer never signs the unreadable-body marker.
	canonical, err := buildCanonicalString(req, timestamp, nonce)
	if err != nil {
		return err
	}

	// 6. Compute HMAC-SHA256
	sig := ComputeHMAC(a.SecretKey, canonical)
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(sig))

	return nil
}

// sizeRequestBody reads the request body into memory and sets
// ContentLength and GetBody from the bytes read, so net/http sends the body
// with a Content-Length that signed endpoints accept (also when the caller
// gave an unsized reader, ContentLength 0 or -1, or a Transfer-Encoding),
// and the signed hash covers exactly the bytes sent. An empty body becomes
// http.NoBody. A read error is returned instead of signing without the body.
//
// A body that is already sized and rewindable (ContentLength > 0, no
// Transfer-Encoding, GetBody set, as http.NewRequest gives a bytes-backed
// body) is left as it is: it is sent with its Content-Length, and
// BuildCanonicalString hashes it; net/http reports a length mismatch.
func sizeRequestBody(req *http.Request) error {
	if req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	if req.ContentLength > 0 && len(req.TransferEncoding) == 0 && req.GetBody != nil {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return fmt.Errorf("failed to read request body for signing: %w", err)
	}
	req.TransferEncoding = nil
	if len(body) == 0 {
		req.Body = http.NoBody
		req.ContentLength = 0
		req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		return nil
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return nil
}

// ErrUnsizedSignedBody is returned by CheckSignedBodyFraming for a signed
// request whose body does not declare its length.
var ErrUnsizedSignedBody = errors.New("signed request body must declare its length")

// CheckSignedBodyFraming reports an error when a signed request's body has
// no declared length (ContentLength -1 or a Transfer-Encoding). Signers
// always send a Content-Length (see HMACAuth.ApplyAuth); verifiers call
// this before checking the signature. BuildCanonicalString covers the body
// either way; this check is a second layer.
func CheckSignedBodyFraming(r *http.Request) error {
	if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
		return ErrUnsizedSignedBody
	}
	return nil
}

// bodyUnreadableMarker is written in place of the body hash when the body
// cannot be read on the verifying side. It starts with '!', which is
// outside the base64 alphabet, so it never equals a body hash, and no
// signer signs it (ApplyAuth fails on a body read error), so verification
// fails.
const bodyUnreadableMarker = "!body-unreadable"

// Refresh indicates that refresh is not supported for HMAC auth.
func (a *HMACAuth) Refresh() (bool, error) { return false, nil }

// BuildCanonicalString builds the canonical string for HMAC signing.
// Format: METHOD\nPATH\nQUERY\nTIMESTAMP\nNONCE\nSIGNED_HEADERS\nBODY_HASH
// It is the only implementation: the signer, the broker verifiers and the
// hub verifiers (pkg/hub/brokerauth.go) all call it.
//
// The body hash line is present exactly when the body has at least one
// byte, whatever ContentLength or Transfer-Encoding say. An empty body has
// no hash line.
//
// When the body cannot be read, the hash is replaced by a marker that no
// signer produces (HMACAuth.ApplyAuth refuses to sign instead), so the
// signature does not verify.
func BuildCanonicalString(r *http.Request, timestamp, nonce string) []byte {
	canonical, err := buildCanonicalString(r, timestamp, nonce)
	if err != nil {
		canonical = append(canonical, bodyUnreadableMarker...)
	}
	return canonical
}

// buildCanonicalString builds the canonical string as BuildCanonicalString
// does, but returns the body read error instead of writing the marker; the
// canonical string returned with an error ends before the body hash.
func buildCanonicalString(r *http.Request, timestamp, nonce string) ([]byte, error) {
	var buf bytes.Buffer

	// HTTP method
	buf.WriteString(r.Method)
	buf.WriteByte('\n')

	// Request path
	buf.WriteString(r.URL.Path)
	buf.WriteByte('\n')

	// Query string (raw, unsorted - matches hub behavior)
	buf.WriteString(r.URL.RawQuery)
	buf.WriteByte('\n')

	// Timestamp
	buf.WriteString(timestamp)
	buf.WriteByte('\n')

	// Nonce
	buf.WriteString(nonce)
	buf.WriteByte('\n')

	// Signed headers (if specified)
	signedHeaders := r.Header.Get(HeaderSignedHeaders)
	if signedHeaders != "" {
		// Headers are listed as semicolon-separated names
		headerNames := strings.Split(signedHeaders, ";")
		for _, name := range headerNames {
			name = strings.TrimSpace(name)
			value := r.Header.Get(name)
			buf.WriteString(strings.ToLower(name))
			buf.WriteByte(':')
			buf.WriteString(strings.TrimSpace(value))
			buf.WriteByte('\n')
		}
	}

	// Body hash (SHA-256 of request body), whenever there is a body.
	if r.Body != nil && r.Body != http.NoBody {
		// Read the body and restore it for the handler.
		// The original body is closed once buffered; the request carries
		// the buffered copy from here on.
		bodyBytes, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		if err != nil {
			return buf.Bytes(), fmt.Errorf("failed to read request body for signing: %w", err)
		}
		if len(bodyBytes) > 0 {
			bodyHash := sha256.Sum256(bodyBytes)
			buf.WriteString(base64.StdEncoding.EncodeToString(bodyHash[:]))
		}
	}

	return buf.Bytes(), nil
}

// ComputeHMAC computes HMAC-SHA256.
func ComputeHMAC(secret, data []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(data)
	return h.Sum(nil)
}

// VerifyHMAC verifies an HMAC-SHA256 signature.
func VerifyHMAC(secret, data, signature []byte) bool {
	expected := ComputeHMAC(secret, data)
	return hmac.Equal(expected, signature)
}

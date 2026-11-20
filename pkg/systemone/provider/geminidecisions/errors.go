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

package geminidecisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
)

const (
	maxRetryAfterDuration = 24 * time.Hour
	retryInfoTypeSuffix   = "/google.rpc.RetryInfo"
	retryInfoTypeShort    = "google.rpc.RetryInfo"
)

// isEgressSentinel reports whether err matches one of pkg/systemone/egress's
// policy or body-cap sentinels. The runtime owns egress-error FailReason
// classification (runtime-design §6.3), so geminidecisions passes these errors
// through unwrapped without assigning a competing ProviderError.Reason.
func isEgressSentinel(err error) bool {
	return egress.IsRedirectBlocked(err) ||
		egress.IsBlockedDestination(err) ||
		egress.IsSchemeNotAllowed(err) ||
		egress.IsInvalidEndpoint(err) ||
		egress.IsResponseTooLarge(err) ||
		egress.IsAuthError(err) ||
		egress.IsCredentialUnavailable(err) ||
		egress.IsInvalidPlacement(err)
}

// classifyTransportError passes pkg/systemone/egress sentinels through raw so
// the runtime classifies them, and maps plain connection / timeout / context
// failures to a content-free *provider.ProviderError without echoing raw error
// strings, URLs, or secrets.
func classifyTransportError(ctx context.Context, err error) error {
	if isEgressSentinel(err) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)):
		return provider.NewProviderError(provider.FailCanceled, false, 0, 0, "provider request canceled")
	case errors.Is(err, context.DeadlineExceeded) || (ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)):
		return provider.NewProviderError(provider.FailTimeout, false, 0, 0, "provider request deadline exceeded")
	default:
		return provider.NewProviderError(provider.FailUnavailable, true, 0, 0, "provider connection or transport failure")
	}
}

type googleErrorEnvelope struct {
	Error *googleErrorBody `json:"error"`
}

type googleErrorBody struct {
	Code    json.RawMessage   `json:"code"`
	Status  string            `json:"status"`
	Details []json.RawMessage `json:"details"`
}

type googleRetryInfoDetail struct {
	Type          string `json:"@type"`
	RetryDelay    string `json:"retryDelay"`
	RetryDelayAlt string `json:"retry_delay"`
	Reason        string `json:"reason"`
}

type parsedGoogleError struct {
	codeString string
	status     string
	reason     string
	retryDelay time.Duration
}

func parseGoogleError(body []byte) parsedGoogleError {
	if len(body) == 0 {
		return parsedGoogleError{}
	}
	var env googleErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return parsedGoogleError{}
	}

	var out parsedGoogleError
	out.status = strings.TrimSpace(env.Error.Status)
	if len(env.Error.Code) > 0 {
		var codeStr string
		if err := json.Unmarshal(env.Error.Code, &codeStr); err == nil {
			out.codeString = strings.TrimSpace(codeStr)
		}
	}
	for _, rawDetail := range env.Error.Details {
		var d googleRetryInfoDetail
		if err := json.Unmarshal(rawDetail, &d); err != nil {
			continue
		}
		if out.reason == "" && strings.TrimSpace(d.Reason) != "" {
			out.reason = strings.TrimSpace(d.Reason)
		}
		typ := strings.TrimSpace(d.Type)
		if typ != retryInfoTypeShort && !strings.HasSuffix(typ, retryInfoTypeSuffix) {
			continue
		}
		rawDelay := strings.TrimSpace(d.RetryDelay)
		if rawDelay == "" {
			rawDelay = strings.TrimSpace(d.RetryDelayAlt)
		}
		if dur := parseProtobufDuration(rawDelay); out.retryDelay == 0 && dur > 0 {
			out.retryDelay = dur
		}
	}
	return out
}

func isDocumentedSafetyBlockCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "safety",
		"prohibited_content",
		"blocklist",
		"image_safety",
		"content_blocked":
		return true
	default:
		return false
	}
}

// classifyHTTPError maps a non-2xx Gemini Developer API HTTP response to a
// content-free *provider.ProviderError per PV3 brief §5.4.
//
// Error response bodies are never copied into ProviderError.Message; bodies are
// inspected solely for documented safety-block string codes, RESOURCE_EXHAUSTED
// status, and google.rpc.RetryInfo.retryDelay durations.
func classifyHTTPError(status int, hdr http.Header, body []byte, now time.Time) *provider.ProviderError {
	parsed := parseGoogleError(body)

	switch status {
	case http.StatusBadRequest:
		if isDocumentedSafetyBlockCode(parsed.codeString) {
			return provider.NewProviderError(
				provider.FailContentBlocked,
				false,
				0,
				status,
				"provider blocked content (HTTP 400)",
			)
		}
		return provider.NewProviderError(
			provider.FailInvalidRequest,
			false,
			0,
			status,
			"provider rejected request (HTTP 400)",
		)

	case http.StatusUnauthorized, http.StatusForbidden:
		msg := fmt.Sprintf("provider authentication rejected (HTTP %d)", status)
		var authHint string
		if strings.EqualFold(parsed.reason, "ACCESS_TOKEN_SCOPE_INSUFFICIENT") {
			msg = fmt.Sprintf("provider authentication rejected: insufficient access token scope (HTTP %d)", status)
			authHint = provider.AuthHintTokenScopeInsufficient
		}
		pe := provider.NewProviderError(
			provider.FailAuth,
			false,
			0,
			status,
			msg,
		)
		pe.AuthHint = authHint
		return pe

	case http.StatusNotFound:
		return provider.NewProviderError(
			provider.FailInvalidRequest,
			false,
			0,
			status,
			"provider model or endpoint not found (HTTP 404)",
		)

	case http.StatusRequestEntityTooLarge:
		return provider.NewProviderError(
			provider.FailInputTooLarge,
			false,
			0,
			status,
			"provider rejected payload size (HTTP 413)",
		)

	case http.StatusTooManyRequests:
		retryAfter := parseRetryAfter(hdr, now)
		if retryAfter <= 0 {
			retryAfter = parsed.retryDelay
		}
		return provider.NewProviderError(
			provider.FailRateLimited,
			true,
			retryAfter,
			status,
			"provider rate limit exceeded (HTTP 429)",
		)

	case http.StatusRequestTimeout:
		retryAfter := parseRetryAfter(hdr, now)
		return provider.NewProviderError(
			provider.FailUnavailable,
			true,
			retryAfter,
			status,
			"provider request timeout (HTTP 408)",
		)

	default:
		if strings.EqualFold(parsed.status, "RESOURCE_EXHAUSTED") {
			retryAfter := parseRetryAfter(hdr, now)
			if retryAfter <= 0 {
				retryAfter = parsed.retryDelay
			}
			return provider.NewProviderError(
				provider.FailRateLimited,
				true,
				retryAfter,
				status,
				fmt.Sprintf("provider resource exhausted (HTTP %d)", status),
			)
		}
		if status >= 500 && status <= 599 {
			retryAfter := parseRetryAfter(hdr, now)
			return provider.NewProviderError(
				provider.FailUnavailable,
				true,
				retryAfter,
				status,
				fmt.Sprintf("provider upstream server error (HTTP %d)", status),
			)
		}
		if status >= 400 && status <= 499 {
			return provider.NewProviderError(
				provider.FailInvalidRequest,
				false,
				0,
				status,
				fmt.Sprintf("provider rejected client request (HTTP %d)", status),
			)
		}
		return provider.NewProviderError(
			provider.FailMalformedResponse,
			false,
			0,
			status,
			fmt.Sprintf("unexpected HTTP status from provider (HTTP %d)", status),
		)
	}
}

func parseProtobufDuration(raw string) time.Duration {
	if raw == "" || !strings.HasSuffix(raw, "s") {
		return 0
	}
	secsRaw := strings.TrimSuffix(raw, "s")
	secs, err := strconv.ParseFloat(secsRaw, 64)
	if err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) || secs <= 0 {
		return 0
	}
	const maxSecs = float64(maxRetryAfterDuration / time.Second)
	if secs >= maxSecs {
		return maxRetryAfterDuration
	}
	return time.Duration(math.Round(secs * float64(time.Second)))
}

func parseRetryAfter(hdr http.Header, now time.Time) time.Duration {
	if hdr == nil {
		return 0
	}
	raRaw := strings.TrimSpace(hdr.Get("Retry-After"))
	if raRaw == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(raRaw, 64); err == nil && !math.IsNaN(secs) && !math.IsInf(secs, 0) {
		if secs <= 0 {
			return 0
		}
		const maxSecs = float64(maxRetryAfterDuration / time.Second)
		if secs >= maxSecs {
			return maxRetryAfterDuration
		}
		return time.Duration(math.Round(secs * float64(time.Second)))
	}
	if retryAt, err := http.ParseTime(raRaw); err == nil {
		d := retryAt.Sub(now)
		if d <= 0 {
			return 0
		}
		if d > maxRetryAfterDuration {
			return maxRetryAfterDuration
		}
		return d
	}
	return 0
}

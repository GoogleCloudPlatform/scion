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

package jevwire

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
)

const tokenMaxTokensExceeded = "max_tokens_exceeded"

// isEgressSentinel reports whether err matches one of pkg/systemone/egress's
// policy or body-cap sentinels. The runtime owns egress-error FailReason
// classification (runtime-design §6.3), so jevwire passes these errors through
// unwrapped without assigning a competing ProviderError.Reason.
func isEgressSentinel(err error) bool {
	return egress.IsRedirectBlocked(err) ||
		egress.IsBlockedDestination(err) ||
		egress.IsSchemeNotAllowed(err) ||
		egress.IsInvalidEndpoint(err) ||
		egress.IsResponseTooLarge(err) ||
		egress.IsAuthError(err)
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

// classifyHTTPError maps a non-2xx Jev-wire HTTP response to a content-free
// *provider.ProviderError per provider-design §7.4 and P1 brief §3.
//
// Error response bodies are never copied into ProviderError.Message; HTTP 400
// bodies are inspected solely for the "max_tokens_exceeded" token in "detail".
func classifyHTTPError(status int, hdr http.Header, body []byte, now time.Time) *provider.ProviderError {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return provider.NewProviderError(provider.FailAuth, false, 0, status, "provider authentication rejected")
	case http.StatusBadRequest:
		if hasMaxTokensExceededToken(body) {
			return provider.NewProviderError(provider.FailInputTooLarge, false, 0, status, "provider rejected input: max_tokens_exceeded")
		}
		return provider.NewProviderError(provider.FailInvalidRequest, false, 0, status, "provider rejected request (HTTP 400)")
	case http.StatusRequestEntityTooLarge:
		return provider.NewProviderError(provider.FailInputTooLarge, false, 0, status, "provider rejected payload size (HTTP 413)")
	case http.StatusUnsupportedMediaType:
		return provider.NewProviderError(provider.FailInvalidRequest, false, 0, status, "provider rejected media type (HTTP 415)")
	case http.StatusUnprocessableEntity:
		return provider.NewProviderError(provider.FailInvalidRequest, false, 0, status, "provider rejected request schema (HTTP 422)")
	case http.StatusTooManyRequests:
		retryAfter := parseRetryAfter(hdr, now)
		return provider.NewProviderError(provider.FailRateLimited, true, retryAfter, status, "provider rate limit exceeded")
	case http.StatusRequestTimeout:
		retryAfter := parseRetryAfter(hdr, now)
		return provider.NewProviderError(provider.FailUnavailable, true, retryAfter, status, "provider request timeout (HTTP 408)")
	case 529:
		retryAfter := parseRetryAfter(hdr, now)
		return provider.NewProviderError(provider.FailUnavailable, true, retryAfter, status, "provider overloaded (HTTP 529)")
	default:
		if status >= 500 && status <= 599 {
			retryAfter := parseRetryAfter(hdr, now)
			return provider.NewProviderError(provider.FailUnavailable, true, retryAfter, status, "provider upstream server error")
		}
		if status >= 400 && status <= 499 {
			return provider.NewProviderError(provider.FailInvalidRequest, false, 0, status, "provider rejected client request")
		}
		return provider.NewProviderError(provider.FailMalformedResponse, false, 0, status, "unexpected HTTP status from provider")
	}
}

// hasMaxTokensExceededToken inspects a 400 HTTPBadRequestError JSON body solely
// to determine whether "detail" (a string or structured object/array) carries
// the "max_tokens_exceeded" token (provider-design §7.4).
func hasMaxTokensExceededToken(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	if detail, ok := payload["detail"]; ok && containsMaxTokensToken(detail, 0) {
		return true
	}
	for _, fallbackKey := range []string{"code", "type", "error"} {
		if v, ok := payload[fallbackKey]; ok && containsMaxTokensToken(v, 0) {
			return true
		}
	}
	return false
}

func containsMaxTokensToken(v any, depth int) bool {
	if depth > 8 || v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.Contains(x, tokenMaxTokensExceeded)
	case map[string]any:
		for _, val := range x {
			if containsMaxTokensToken(val, depth+1) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if containsMaxTokensToken(item, depth+1) {
				return true
			}
		}
	}
	return false
}

// parseRetryAfter parses retry-after-ms (preferred, millisecond precision) or
// standard Retry-After (delta-seconds or HTTP-date) into a non-negative Duration.
func parseRetryAfter(hdr http.Header, now time.Time) time.Duration {
	if hdr == nil {
		return 0
	}
	if msRaw := strings.TrimSpace(hdr.Get("retry-after-ms")); msRaw != "" {
		if ms, err := strconv.ParseFloat(msRaw, 64); err == nil && !math.IsNaN(ms) && !math.IsInf(ms, 0) && ms > 0 {
			const maxMillis = float64(24 * time.Hour / time.Millisecond)
			if ms > maxMillis {
				ms = maxMillis
			}
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	raRaw := strings.TrimSpace(hdr.Get("Retry-After"))
	if raRaw == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(raRaw, 64); err == nil && !math.IsNaN(secs) && !math.IsInf(secs, 0) {
		if secs <= 0 {
			return 0
		}
		const maxSecs = float64(24 * time.Hour / time.Second)
		if secs > maxSecs {
			secs = maxSecs
		}
		return time.Duration(secs * float64(time.Second))
	}
	if retryAt, err := http.ParseTime(raRaw); err == nil {
		d := retryAt.Sub(now)
		if d <= 0 {
			return 0
		}
		if d > 24*time.Hour {
			return 24 * time.Hour
		}
		return d
	}
	return 0
}

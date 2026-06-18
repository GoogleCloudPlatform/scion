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

package lifecyclehooks

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Variable trust classification
// ---------------------------------------------------------------------------

// VarTrust represents the trust class of a substitution variable.
type VarTrust int

const (
	// Trusted variables are admin/platform-fixed: hook config values,
	// project metadata, hub-controlled agent identity fields.
	Trusted VarTrust = iota

	// Untrusted variables are agent/runtime-derived: AGENT_NAME,
	// TASK_SUMMARY, or anything influenced by the LLM/agent.
	Untrusted
)

// TrustedVars is the set of variables classified as TRUSTED (admin/platform-fixed).
// Unknown variables default to UNTRUSTED.
var TrustedVars = map[string]VarTrust{
	// Hook config (admin-set at creation time)
	"HOOK_ID":   Trusted,
	"HOOK_NAME": Trusted,
	"TRIGGER":   Trusted,

	// Project metadata (Hub-controlled)
	"PROJECT_ID":   Trusted,
	"PROJECT_NAME": Trusted,
	"PROJECT_SLUG": Trusted,

	// Hub-controlled agent identity (set by Hub, not agent)
	"AGENT_ID":   Trusted,
	"AGENT_SLUG": Trusted,

	// Execution identity (Hub-resolved SA)
	"SA_EMAIL": Trusted,
}

// UntrustedVars is the set of variables explicitly classified as UNTRUSTED
// (agent/runtime-derived, LLM-influenced).
var UntrustedVars = map[string]VarTrust{
	"AGENT_NAME":   Untrusted,
	"TASK_SUMMARY": Untrusted,
	"AGENT_STATUS": Untrusted,
	"ERROR_MSG":    Untrusted,
}

// ClassifyVar returns the trust class for a variable name. Unknown variables
// default to Untrusted.
func ClassifyVar(name string) VarTrust {
	if trust, ok := TrustedVars[name]; ok {
		return trust
	}
	if trust, ok := UntrustedVars[name]; ok {
		return trust
	}
	// Unknown variables default to UNTRUSTED — security-conservative default.
	return Untrusted
}

// ---------------------------------------------------------------------------
// Variable pattern
// ---------------------------------------------------------------------------

// varPattern matches ${VARIABLE_NAME} substitution placeholders.
var varPattern = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// extractVars returns all unique variable names found in s.
func extractVars(s string) []string {
	matches := varPattern.FindAllStringSubmatch(s, -1)
	seen := make(map[string]bool, len(matches))
	var vars []string
	for _, m := range matches {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			vars = append(vars, name)
		}
	}
	return vars
}

// ---------------------------------------------------------------------------
// Static validation (create/update time)
// ---------------------------------------------------------------------------

// ValidateActionVariables checks that no untrusted variable appears in a
// disallowed position within the action template. This is the static
// (create/update time) half of the untrusted-variable guard.
//
// Rules enforced:
//   - Untrusted vars NEVER in URL host or path (SSRF risk).
//   - Untrusted vars NEVER in URL query params.
//   - Untrusted vars NEVER in any header value (auth or non-auth).
//   - Untrusted vars NEVER in any header name.
//   - Untrusted vars are rejected EVERYWHERE unless explicitly allow-listed
//     by the admin in action.AllowedUntrustedVars.
//   - Even allow-listed untrusted vars are allowed ONLY in the body
//     (never URL host/path, query, or headers).
//   - Allow-listed untrusted vars in the body must sit inside a JSON string
//     literal (immediately wrapped by double quotes in the template).
//   - Body is assumed to be JSON; non-JSON content types are not yet supported
//     (see C8 note below).
//
// C8: Content-type awareness is limited. The body is assumed to be JSON and
// untrusted variables are JSON-string-encoded at render time. If the body is
// not JSON (e.g. form-encoded), the encoding may be inappropriate. A future
// enhancement may key off a Content-Type header to select the encoding.
func ValidateActionVariables(a *store.LifecycleHookAction) []FieldError {
	var errs []FieldError

	// Build allow-list set for O(1) lookup.
	allowed := make(map[string]bool, len(a.AllowedUntrustedVars))
	for _, v := range a.AllowedUntrustedVars {
		allowed[v] = true
	}

	if a.URL != "" {
		errs = append(errs, validateURLVariables(a.URL, allowed)...)
	}

	// Header names must never contain variables (any trust level could be
	// used to inject new headers).
	for name := range a.Headers {
		for _, v := range extractVars(name) {
			errs = append(errs, FieldError{
				Field:   fmt.Sprintf("action.headers[%s]", name),
				Message: fmt.Sprintf("variable ${%s} not allowed in header name", v),
			})
		}
	}

	// B1: ALL header values must reject untrusted variables, not just auth
	// headers. Headers are security-sensitive (can carry credentials,
	// routing info, CORS directives, etc.).
	for name, value := range a.Headers {
		for _, v := range extractVars(value) {
			if ClassifyVar(v) == Untrusted {
				if !allowed[v] {
					errs = append(errs, FieldError{
						Field:   fmt.Sprintf("action.headers[%s]", name),
						Message: fmt.Sprintf("untrusted variable ${%s} not allowed in header value (not in AllowedUntrustedVars)", v),
					})
				} else {
					// Even allow-listed untrusted vars are forbidden in headers.
					errs = append(errs, FieldError{
						Field:   fmt.Sprintf("action.headers[%s]", name),
						Message: fmt.Sprintf("untrusted variable ${%s} not allowed in header value (allowed only in body)", v),
					})
				}
			}
		}
	}

	// Body: untrusted variables are allowed only if they are in the
	// allow-list AND sit inside a JSON string literal.
	if a.Body != "" {
		errs = append(errs, validateBodyVariables(a.Body, allowed)...)
	}

	return errs
}

// validateURLVariables checks variable placement within the URL template.
// Untrusted variables are forbidden everywhere in the URL (host, path, query).
func validateURLVariables(rawURL string, allowed map[string]bool) []FieldError {
	var errs []FieldError

	// Split on '?' to separate host+path from query string.
	parts := strings.SplitN(rawURL, "?", 2)
	hostAndPath := parts[0]

	// Check host+path for untrusted variables.
	for _, v := range extractVars(hostAndPath) {
		if ClassifyVar(v) == Untrusted {
			if !allowed[v] {
				errs = append(errs, FieldError{
					Field:   "action.url",
					Message: fmt.Sprintf("untrusted variable ${%s} not allowed in URL host or path (SSRF risk; not in AllowedUntrustedVars)", v),
				})
			} else {
				errs = append(errs, FieldError{
					Field:   "action.url",
					Message: fmt.Sprintf("untrusted variable ${%s} not allowed in URL host or path (SSRF risk; allowed only in body)", v),
				})
			}
		}
	}

	// Query params: untrusted variables are now also rejected here.
	if len(parts) > 1 {
		query := parts[1]
		for _, v := range extractVars(query) {
			if ClassifyVar(v) == Untrusted {
				if !allowed[v] {
					errs = append(errs, FieldError{
						Field:   "action.url",
						Message: fmt.Sprintf("untrusted variable ${%s} not allowed in URL query (not in AllowedUntrustedVars)", v),
					})
				} else {
					errs = append(errs, FieldError{
						Field:   "action.url",
						Message: fmt.Sprintf("untrusted variable ${%s} not allowed in URL query (allowed only in body)", v),
					})
				}
			}
		}
	}

	return errs
}

// validateBodyVariables checks that untrusted variables in the body are
// (a) in the allow-list and (b) sit inside a JSON string literal — i.e. the
// placeholder is immediately preceded by " and immediately followed by " or
// other content within a JSON string. Concretely, we require the character
// immediately before ${VAR} to be a double quote OR that the placeholder is
// embedded within a JSON string context (preceded by ": " and quote).
//
// B5: This prevents type confusion where an untrusted value appears in a
// non-string JSON position (key, numeric, boolean, null) and could alter
// the JSON structure even after encoding.
func validateBodyVariables(body string, allowed map[string]bool) []FieldError {
	var errs []FieldError

	matches := varPattern.FindAllStringSubmatchIndex(body, -1)
	for _, loc := range matches {
		// loc[0]:loc[1] is the full match ${VAR}
		// loc[2]:loc[3] is the capture group (VAR name)
		varName := body[loc[2]:loc[3]]

		if ClassifyVar(varName) != Untrusted {
			continue
		}

		if !allowed[varName] {
			errs = append(errs, FieldError{
				Field:   "action.body",
				Message: fmt.Sprintf("untrusted variable ${%s} not allowed (not in AllowedUntrustedVars)", varName),
			})
			continue
		}

		// B5: Check that the placeholder sits inside a JSON string literal.
		// The character immediately before ${VAR} must be a double quote (")
		// indicating we're inside a "..." string value, OR we look back to
		// confirm the context is within quotes.
		if !isInsideJSONString(body, loc[0]) {
			errs = append(errs, FieldError{
				Field:   "action.body",
				Message: fmt.Sprintf("untrusted variable ${%s} must be inside a JSON string literal (quoted); found in non-string position", varName),
			})
		}
	}

	return errs
}

// isInsideJSONString checks whether position pos in s is inside a JSON string
// literal. It counts unescaped double quotes before pos; an odd count means
// we are inside a string.
func isInsideJSONString(s string, pos int) bool {
	quoteCount := 0
	for i := 0; i < pos; i++ {
		if s[i] == '\\' {
			i++ // skip escaped character
			continue
		}
		if s[i] == '"' {
			quoteCount++
		}
	}
	return quoteCount%2 == 1
}

// ---------------------------------------------------------------------------
// Renderer (execution time)
// ---------------------------------------------------------------------------

// RenderVars is the variable values to substitute at execution time.
type RenderVars map[string]string

// RenderAction renders a LifecycleHookAction template by substituting
// variables with their values from vars. Untrusted variable values are
// strictly encoded:
//   - In body: JSON-string-encoded (escaped for safe embedding in JSON).
//
// Trusted variables are substituted verbatim in URL and body positions.
//
// B2: Header rendering applies defense-in-depth: untrusted variables are
// refused (skipped/blanked) even if the static validator were bypassed.
// Additionally, CR/LF characters are stripped from all header values to
// prevent header injection.
//
// This is the execution-time half of the untrusted-variable guard. The
// static validator (ValidateActionVariables) has already rejected any hook
// that places an untrusted variable in a disallowed position, so this
// function provides a defense-in-depth layer.
//
// Returns a new LifecycleHookAction with all variables resolved. Variables
// not present in vars are left as-is (the caller decides whether to treat
// that as an error).
func RenderAction(a *store.LifecycleHookAction, vars RenderVars) *store.LifecycleHookAction {
	rendered := &store.LifecycleHookAction{
		Type:                 a.Type,
		Method:               a.Method,
		OnError:              a.OnError,
		TimeoutSeconds:       a.TimeoutSeconds,
		AllowedUntrustedVars: a.AllowedUntrustedVars,
	}

	// Render URL with position-aware encoding.
	rendered.URL = renderURL(a.URL, vars)

	// B2: Render headers with defense-in-depth — refuse untrusted vars and
	// strip CR/LF from all substituted values.
	if a.Headers != nil {
		rendered.Headers = make(map[string]string, len(a.Headers))
		for name, value := range a.Headers {
			rendered.Headers[name] = renderHeaderValue(value, vars)
		}
	}

	// Render body — untrusted vars are JSON-string-encoded.
	rendered.Body = renderBody(a.Body, vars)

	return rendered
}

// renderURL substitutes variables in a URL template. Host/path variables
// (which must be trusted, per static validation) are substituted verbatim.
// Query-parameter values are also substituted verbatim for trusted vars
// (untrusted vars in query are rejected at validation time).
func renderURL(rawURL string, vars RenderVars) string {
	parts := strings.SplitN(rawURL, "?", 2)
	hostAndPath := parts[0]

	// Host+path: only trusted vars are allowed (enforced statically).
	// Substitute verbatim.
	hostAndPath = renderTrustedSubstitution(hostAndPath, vars)

	if len(parts) == 1 {
		return hostAndPath
	}

	// Query string: untrusted vars are now rejected at validation time,
	// but for defense-in-depth, we still percent-encode untrusted values
	// if any slip through.
	query := parts[1]
	query = varPattern.ReplaceAllStringFunc(query, func(match string) string {
		name := varPattern.FindStringSubmatch(match)[1]
		value, ok := vars[name]
		if !ok {
			return match // Leave unresolved.
		}
		if ClassifyVar(name) == Untrusted {
			return url.QueryEscape(value)
		}
		return value
	})

	return hostAndPath + "?" + query
}

// renderTrustedSubstitution substitutes variables in positions where only
// trusted variables are allowed (enforced at static validation time).
// D1 defense-in-depth: untrusted variables are blanked (replaced with empty
// string) even if the static validator were somehow bypassed, matching the
// defense-in-depth pattern used in renderHeaderValue.
func renderTrustedSubstitution(s string, vars RenderVars) string {
	return varPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := varPattern.FindStringSubmatch(match)[1]
		value, ok := vars[name]
		if !ok {
			return match
		}
		// D1: Blank untrusted vars in URL host/path as defense-in-depth.
		if ClassifyVar(name) == Untrusted {
			return ""
		}
		return value
	})
}

// renderHeaderValue substitutes variables in a header value with
// defense-in-depth protections:
//   - Untrusted variables are blanked (replaced with empty string) rather
//     than substituted, even if the static validator were bypassed.
//   - The fully rendered value has CR (\r) and LF (\n) stripped to prevent
//     HTTP header injection — sanitization is applied after all substitutions
//     so CR/LF in the static template (or introduced by concatenation) is also
//     removed, not just CR/LF inside individual variable values.
func renderHeaderValue(s string, vars RenderVars) string {
	rendered := varPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := varPattern.FindStringSubmatch(match)[1]
		value, ok := vars[name]
		if !ok {
			return match
		}
		// B2: Defense-in-depth — refuse untrusted variables in headers.
		if ClassifyVar(name) == Untrusted {
			return "" // Blank untrusted values at render time.
		}
		return value
	})
	return sanitizeHeaderValue(rendered)
}

// sanitizeHeaderValue removes CR and LF characters from a header value
// to prevent HTTP header injection attacks.
func sanitizeHeaderValue(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

// renderBody substitutes variables in a body template. Untrusted variable
// values are JSON-string-encoded (double-quote-escaped) to prevent JSON
// structure injection. Trusted variables are substituted verbatim.
//
// NOTE (C8): The body is assumed to be JSON. If the body uses a different
// content type (e.g. form-encoded), JSON-string encoding may be inappropriate.
// A future enhancement may key off a Content-Type header to select encoding.
func renderBody(body string, vars RenderVars) string {
	return varPattern.ReplaceAllStringFunc(body, func(match string) string {
		name := varPattern.FindStringSubmatch(match)[1]
		value, ok := vars[name]
		if !ok {
			return match
		}
		if ClassifyVar(name) == Untrusted {
			return jsonEncodeValue(value)
		}
		return value
	})
}

// jsonEncodeValue JSON-encodes a string value for safe embedding in a JSON
// body. It marshals the value as a JSON string and strips the surrounding
// quotes so the result can be placed inside an existing JSON string literal.
// This prevents JSON structure injection (e.g., closing a string and adding
// new fields via \" or similar).
func jsonEncodeValue(s string) string {
	b, _ := json.Marshal(s)
	// json.Marshal wraps in quotes: "value". Strip them so the result
	// can be embedded inside a JSON string literal in the template.
	encoded := string(b)
	if len(encoded) >= 2 && encoded[0] == '"' && encoded[len(encoded)-1] == '"' {
		return encoded[1 : len(encoded)-1]
	}
	return encoded
}

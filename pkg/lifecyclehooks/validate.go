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

// Package lifecyclehooks provides validation and variable-substitution logic
// for lifecycle hooks. It is imported by both the Hub API handlers (create/update
// validation) and the executor (render-time variable guard). It depends on
// pkg/store for model types (LifecycleHookAction, constants, etc.).
package lifecyclehooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MaxTimeoutSeconds is the validated maximum per-action timeout.
// Hooks with a timeout exceeding this are rejected at validation time.
const MaxTimeoutSeconds = 30

// validTriggers is the set of authoritative phase transitions supported in v1.
var validTriggers = map[string]bool{
	store.LifecycleHookTriggerRunning:   true,
	store.LifecycleHookTriggerSuspended: true,
	store.LifecycleHookTriggerStopped:   true,
	store.LifecycleHookTriggerError:     true,
}

// validActionTypes is the set of action types supported in v1.
var validActionTypes = map[string]bool{
	store.LifecycleHookActionHTTP:    true,
	store.LifecycleHookActionWebhook: true,
}

// validHTTPMethods is the set of HTTP methods allowed in hook actions.
var validHTTPMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodHead:   true,
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// validOnErrorPolicies is the set of on_error failure policies.
var validOnErrorPolicies = map[string]bool{
	store.LifecycleHookOnErrorLog:   true,
	store.LifecycleHookOnErrorRetry: true,
}

// authHeaderNames lists header names that are considered authentication
// or credential-carrying headers. Comparison is case-insensitive.
var authHeaderNames = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"x-auth-token":        true,
	"cookie":              true,
	"set-cookie":          true,
}

// ValidationError collects one or more field-level validation failures.
type ValidationError struct {
	Errors []FieldError
}

// FieldError describes a single field validation failure.
type FieldError struct {
	Field   string // Dotted path, e.g. "action.url"
	Message string
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 1 {
		return fmt.Sprintf("validation error: %s: %s", e.Errors[0].Field, e.Errors[0].Message)
	}
	msgs := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		msgs[i] = fmt.Sprintf("%s: %s", fe.Field, fe.Message)
	}
	return fmt.Sprintf("validation errors: %s", strings.Join(msgs, "; "))
}

// IsValidationError reports whether err is a *ValidationError.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

// GCPServiceAccountResolver looks up a GCP service account by ID. Callers
// provide an implementation backed by the store; this package has no store
// dependency.
type GCPServiceAccountResolver interface {
	GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error)
}

// SurfaceHookExecutionIdentity names this surface in audit records and in the
// startup warning that accompanies a disabled checker.
//
// ⚠️ IT DEGRADES DIFFERENTLY FROM THE AGENT-ASSIGN SURFACE, which is why the
// two are named separately rather than covered by one message about "the IAM
// check". Agent assignment also has a Hub policy layer (ActionAssign), so a
// disabled checker there leaves it POLICY-GATED. Here there is no second layer:
// setting execution_identity is gated by the caller's ability to write the hook
// and by the scope and verification checks below, none of which asks whether
// the caller may act as the account. With the checker disabled this surface is
// UNGATED with respect to actAs.
const SurfaceHookExecutionIdentity = "hook-execution-identity"

// ValidateHook validates a LifecycleHook for correctness before persist or
// update. It checks structural well-formedness, trigger/action validity,
// execution-identity resolution, the caller's permission to act as that
// identity, and the untrusted-variable guard for the action template.
//
// saResolver may be nil only when execution_identity is empty (webhook with
// no identity). If saResolver is nil and execution_identity is non-empty,
// a validation error is returned.
//
// caller, actAs and audit are REQUIRED, and are positional parameters rather
// than optional fields on purpose: adding them to a struct or defaulting them
// would let a new call site acquire the ungated behaviour by omission. Every
// caller is forced by the compiler to say who is asking, what should check
// them, and where the record goes. A nil actAs denies
// (store.MechanismCheckUnwired); switching the check off is done by passing
// store.NewDisabledCallerPermissionChecker, which is a value somebody has to
// construct.
//
// audit is the §7 sink. A nil audit WARNS and proceeds rather than denying,
// which is deliberately unlike a nil actAs: a missing checker means we do not
// know whether the caller is permitted, while a missing sink means we know and
// cannot file it. Failing closed on the latter would turn a logging
// misconfiguration into an outage.
//
// ⚠️ THIS AUTHORIZATION IS WRITE-TIME ONLY, AND THAT IS A RESIDUAL RISK, NOT A
// DESIGN GOAL. The permission is evaluated when a hook is created or updated
// and never again. The executor resolves the identity and mints a token
// (lifecycle_hook_executor.go resolveIdentityAndToken) without re-checking
// either this permission or sa.Verified. Two consequences follow, and neither
// is fixed here:
//
//   - Enabling the check later is NOT a kill switch. Hooks written while it was
//     off keep executing with their existing identities, because nothing
//     re-evaluates them. The toggle governs new writes only.
//   - Revoking a caller's actAs grant in IAM does not stop hooks they already
//     wrote.
//
// Closing this requires an execution-time check in the executor and is tracked
// separately. Do not read the presence of this gate as meaning a running hook's
// identity has been authorized recently.
func ValidateHook(
	ctx context.Context,
	hook *store.LifecycleHook,
	saResolver GCPServiceAccountResolver,
	caller store.Principal,
	actAs store.CallerPermissionChecker,
	audit store.SAAssignmentAuditSink,
) error {
	var errs []FieldError

	// Default an empty scope to hub (matching the store default) BEFORE the
	// checks below. Otherwise an empty ScopeType would silently bypass the
	// execution-identity scope validation (which keys off ScopeType).
	if hook.ScopeType == "" {
		hook.ScopeType = store.LifecycleHookScopeHub
	}

	// --- trigger ---
	if !validTriggers[hook.Trigger] {
		errs = append(errs, FieldError{
			Field:   "trigger",
			Message: fmt.Sprintf("must be one of: running, suspended, stopped, error; got %q", hook.Trigger),
		})
	}

	// --- scope_type / scope_id ---
	// An empty scope_type defaults to "hub" at the store layer. Reject any
	// other unknown value here so it surfaces as a 400 validation error rather
	// than a generic 500 from a downstream ent validation failure.
	if hook.ScopeType != "" &&
		hook.ScopeType != store.LifecycleHookScopeHub &&
		hook.ScopeType != store.LifecycleHookScopeProject {
		errs = append(errs, FieldError{
			Field:   "scopeType",
			Message: fmt.Sprintf("must be one of: hub, project; got %q", hook.ScopeType),
		})
	}
	if hook.ScopeType == store.LifecycleHookScopeProject && hook.ScopeID == "" {
		errs = append(errs, FieldError{
			Field:   "scopeId",
			Message: "required when scopeType is project",
		})
	}

	// --- action ---
	if hook.Action == nil {
		errs = append(errs, FieldError{Field: "action", Message: "required"})
	} else {
		errs = append(errs, validateAction(hook.Action, hook.ExecutionIdentity)...)
	}

	// --- execution_identity ---
	if hook.Action != nil {
		errs = append(errs, validateExecutionIdentity(ctx, hook, saResolver, caller, actAs, audit)...)
	}

	// --- untrusted-variable guard (static, create/update time) ---
	if hook.Action != nil {
		if varErrs := ValidateActionVariables(hook.Action); len(varErrs) > 0 {
			errs = append(errs, varErrs...)
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// validateAction checks action-level well-formedness.
func validateAction(a *store.LifecycleHookAction, execIdentity string) []FieldError {
	var errs []FieldError

	// -- type --
	if !validActionTypes[a.Type] {
		errs = append(errs, FieldError{
			Field:   "action.type",
			Message: fmt.Sprintf("must be one of: http, webhook; got %q", a.Type),
		})
		// Can't validate type-specific rules without a valid type; return early.
		return errs
	}

	// -- method --
	// Both http and webhook actions require canonical uppercase HTTP methods
	// (e.g. "POST", not "post"). This makes the rule consistent across types.
	if a.Type == store.LifecycleHookActionWebhook {
		// Webhook is always POST; if method is set it must be POST (canonical).
		if a.Method != "" && a.Method != http.MethodPost {
			errs = append(errs, FieldError{
				Field:   "action.method",
				Message: fmt.Sprintf("webhook actions must use POST (canonical uppercase); got %q", a.Method),
			})
		}
	} else {
		// http action requires a valid method (must be canonical uppercase per HTTP spec).
		if a.Method == "" {
			errs = append(errs, FieldError{Field: "action.method", Message: "required for http actions"})
		} else if !validHTTPMethods[a.Method] {
			errs = append(errs, FieldError{
				Field:   "action.method",
				Message: fmt.Sprintf("must be one of: GET, HEAD, POST, PUT, PATCH, DELETE; got %q", a.Method),
			})
		}
	}

	// -- url --
	if a.URL == "" {
		errs = append(errs, FieldError{Field: "action.url", Message: "required"})
	} else {
		errs = append(errs, validateActionURL(a.URL)...)
		// S2: http action type requires https (bearer token attached).
		errs = append(errs, validateActionURLSchemeForType(a.URL, a.Type)...)
	}

	// -- headers --
	errs = append(errs, validateHeaders(a)...)

	// -- timeout --
	if a.TimeoutSeconds <= 0 {
		errs = append(errs, FieldError{
			Field:   "action.timeoutSeconds",
			Message: "required and must be > 0",
		})
	} else if a.TimeoutSeconds > MaxTimeoutSeconds {
		errs = append(errs, FieldError{
			Field:   "action.timeoutSeconds",
			Message: fmt.Sprintf("must not exceed %d seconds; got %d", MaxTimeoutSeconds, a.TimeoutSeconds),
		})
	}

	// -- on_error --
	// Default empty on_error to "log" (the design default). This normalization
	// ensures downstream consumers never need to treat empty as a separate case.
	if a.OnError == "" {
		a.OnError = store.LifecycleHookOnErrorLog
	}
	if !validOnErrorPolicies[a.OnError] {
		errs = append(errs, FieldError{
			Field:   "action.onError",
			Message: fmt.Sprintf("must be one of: log, retry; got %q", a.OnError),
		})
	}

	// -- type-specific rules --
	if a.Type == store.LifecycleHookActionHTTP {
		if execIdentity == "" {
			errs = append(errs, FieldError{
				Field:   "executionIdentity",
				Message: "required for http action type",
			})
		}
	}

	if a.Type == store.LifecycleHookActionWebhook {
		// Webhook = unauthenticated POST whose URL carries its own token.
		// Reject auth headers on webhooks.
		if a.Headers != nil {
			for name := range a.Headers {
				if authHeaderNames[strings.ToLower(strings.TrimSpace(name))] {
					errs = append(errs, FieldError{
						Field:   fmt.Sprintf("action.headers[%s]", name),
						Message: "authentication headers are not allowed on webhook actions (webhook URLs carry their own token)",
					})
				}
			}
		}
	}

	return errs
}

// validateActionURL validates the URL template. At validation time, variables
// have not been substituted, so we strip ${VAR} placeholders before parsing to
// check structural validity. The URL must be absolute (scheme + host).
func validateActionURL(rawURL string) []FieldError {
	// Replace variable placeholders with a safe sentinel for URL parsing.
	sanitized := varPattern.ReplaceAllString(rawURL, "PLACEHOLDER")

	u, err := url.Parse(sanitized)
	if err != nil {
		return []FieldError{{Field: "action.url", Message: fmt.Sprintf("invalid URL: %v", err)}}
	}
	if u.Scheme == "" || u.Host == "" {
		return []FieldError{{Field: "action.url", Message: "must be an absolute URL with scheme and host"}}
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return []FieldError{{Field: "action.url", Message: fmt.Sprintf("scheme must be http or https; got %q", u.Scheme)}}
	}
	return nil
}

// validateActionURLSchemeForType checks that the URL scheme is appropriate for
// the action type. S2: http actions REQUIRE https (bearer token attached);
// webhook actions allow http (no bearer token attached).
func validateActionURLSchemeForType(rawURL, actionType string) []FieldError {
	// Strip variable placeholders for parsing.
	sanitized := varPattern.ReplaceAllString(rawURL, "PLACEHOLDER")
	u, err := url.Parse(sanitized)
	if err != nil {
		return nil // structural error already caught by validateActionURL
	}
	if actionType == store.LifecycleHookActionHTTP && u.Scheme == "http" {
		return []FieldError{{
			Field:   "action.url",
			Message: "http action type requires https (bearer token would be sent in cleartext over http)",
		}}
	}
	return nil
}

// validateHeaders checks header names for injection attacks.
func validateHeaders(a *store.LifecycleHookAction) []FieldError {
	var errs []FieldError
	for name := range a.Headers {
		// Header names must not contain control characters, colons, or newlines.
		if !isValidHeaderName(name) {
			errs = append(errs, FieldError{
				Field:   fmt.Sprintf("action.headers[%s]", name),
				Message: "invalid header name: must be a valid HTTP token (no control characters, spaces, or special characters)",
			})
		}
	}
	return errs
}

// isValidHeaderName checks that a header name is a valid HTTP token per RFC 7230.
// Non-ASCII runes (c > 127) are rejected before the byte-level token check to
// avoid truncation of multi-byte runes to a single byte.
func isValidHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c > 127 {
			return false
		}
		if !isTokenChar(byte(c)) {
			return false
		}
	}
	return true
}

// isTokenChar reports whether c is a valid HTTP token character per RFC 7230 §3.2.6.
func isTokenChar(c byte) bool {
	// token = 1*tchar
	// tchar = "!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." /
	//         "^" / "_" / "`" / "|" / "~" / DIGIT / ALPHA
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// validateExecutionIdentity checks that execution_identity references a valid,
// verified GCP service account within the hook's scope, and that the caller is
// permitted to act as it.
//
// The scope and verification checks answer "is this account USABLE here". The
// actAs check answers "may THIS CALLER use it". They are independent questions
// and both must pass — an account can be perfectly in-scope and verified and
// still be one the caller has no business running as.
func validateExecutionIdentity(
	ctx context.Context,
	hook *store.LifecycleHook,
	resolver GCPServiceAccountResolver,
	caller store.Principal,
	actAs store.CallerPermissionChecker,
	audit store.SAAssignmentAuditSink,
) []FieldError {
	if hook.ExecutionIdentity == "" {
		// Webhook actions allow empty execution_identity.
		if hook.Action != nil && hook.Action.Type == store.LifecycleHookActionWebhook {
			return nil
		}
		// For http, the error is already reported in validateAction.
		return nil
	}

	if resolver == nil {
		return []FieldError{{
			Field:   "executionIdentity",
			Message: "cannot validate execution identity: no resolver provided",
		}}
	}

	sa, err := resolver.GetGCPServiceAccount(ctx, hook.ExecutionIdentity)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return []FieldError{{
				Field:   "executionIdentity",
				Message: fmt.Sprintf("GCP service account %q not found", hook.ExecutionIdentity),
			}}
		}
		return []FieldError{{
			Field:   "executionIdentity",
			Message: fmt.Sprintf("failed to resolve GCP service account: %v", err),
		}}
	}

	var errs []FieldError

	// Must be verified.
	if !sa.Verified || sa.VerificationStatus != store.GCPVerificationVerified {
		errs = append(errs, FieldError{
			Field:   "executionIdentity",
			Message: fmt.Sprintf("GCP service account %q is not verified (status: %s)", sa.Email, sa.VerificationStatus),
		})
	}

	// Must be in scope. For hub-scoped hooks, any hub-scoped SA is valid.
	// For project-scoped hooks, the SA must be in the same project scope.
	switch hook.ScopeType {
	case store.LifecycleHookScopeHub:
		// Hub-scoped hooks can use hub-scoped SAs.
		if sa.Scope != "hub" {
			errs = append(errs, FieldError{
				Field:   "executionIdentity",
				Message: fmt.Sprintf("hub-scoped hook requires a hub-scoped service account; SA %q has scope %q", sa.Email, sa.Scope),
			})
		}
	case store.LifecycleHookScopeProject:
		// Project-scoped hooks require the SA to be in the same project.
		if sa.Scope != "project" || sa.ScopeID != hook.ScopeID {
			errs = append(errs, FieldError{
				Field:   "executionIdentity",
				Message: fmt.Sprintf("project-scoped hook requires a service account in the same project; SA %q has scope %s/%s", sa.Email, sa.Scope, sa.ScopeID),
			})
		}
	}

	// --- caller permission (actAs) ---
	//
	// Runs regardless of whether the checks above produced errors, and
	// deliberately so: if it ran only on an otherwise-clean hook, the
	// permission verdict would depend on unrelated validation state, and a
	// caller could learn which accounts exist by watching this check appear and
	// disappear. Validation collects field errors rather than short-circuiting,
	// so reporting both "not verified" and "not permitted" is the normal shape.
	//
	// The decision sequence is store.EvaluateActAs, shared with the agent
	// service-account assignment surface in pkg/hub. This surface chooses only
	// how to report the result. Do not reimplement the ordering here.
	result, err := store.EvaluateActAs(ctx, store.ActAsGate{
		Checker: actAs,
		Caller:  caller,
		// Constant, not a parameter: this package has exactly one actAs
		// surface, so there is nothing for a call site to get wrong or to
		// leave blank.
		Surface: SurfaceHookExecutionIdentity,
		Audit:   audit,
	}, sa)
	if err != nil {
		// Diagnostic only; EvaluateActAs has already forced the outcome to
		// indeterminate, which fails the check below.
		slog.Warn("lifecycle hook execution identity: caller-permission check failed",
			"surface", SurfaceHookExecutionIdentity, "hook", hook.ID,
			"caller", caller.ID, "targetSA", sa.Email, "error", err.Error())
	}
	if result.Outcome != store.ActAsAllowed {
		slog.Warn("lifecycle hook execution identity denied",
			"surface", SurfaceHookExecutionIdentity, "hook", hook.ID,
			"callerKind", caller.Kind.String(), "caller", caller.ID,
			"targetSA", sa.Email, "outcome", result.Outcome.String(),
			"mechanism", result.Mechanism, "reason", result.Reason)
		errs = append(errs, FieldError{
			Field: "executionIdentity",
			Message: fmt.Sprintf(
				"you don't have permission to run hooks as service account %q (%s is required): %s",
				sa.Email, store.PermissionActAs, result.Reason),
		})
	} else {
		// Recorded on the allow path too: "allowed because IAM said so" and
		// "allowed because nobody asked" are the same outcome and different
		// facts, and only the audit record preserves the difference.
		slog.Info("lifecycle hook execution identity allowed",
			"surface", SurfaceHookExecutionIdentity, "hook", hook.ID,
			"callerKind", caller.Kind.String(), "caller", caller.ID,
			"targetSA", sa.Email, "mechanism", result.Mechanism)
	}

	return errs
}

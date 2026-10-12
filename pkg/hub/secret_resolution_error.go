// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"errors"
	"net/http"
)

// secretResolutionError is the error a dispatch returns when the secret
// backend fails to resolve an agent's secrets, or when the hub fails to read
// the agent's stored environment settings (Source says which). It is a read
// or resolution error, never an absence: an agent with no secrets or no
// stored settings resolves to an empty set without error. The dispatch
// stops before anything is sent to the broker and before any token is
// minted, so the start did not happen.
//
// On a cross-node dispatch only a marker travels on the failed row
// (dispatchFailureEnvelope.SecretResolution, plus SecretResolutionSource);
// the requesting node rebuilds the error with a nil Err.
//
// Error() is a fixed message that names no secret, value or backend
// detail: it is what reaches the API caller and the agent's status
// message. The backend error is kept in Err for errors.Is/As and for the
// server-side log only.
type secretResolutionError struct {
	// Verb is the action that was not performed: "started", "restarted"
	// or "created".
	Verb string
	// Source is what could not be read: resolutionSourceSecrets (the zero
	// value) or resolutionSourceEnvSettings. It picks the fixed message.
	Source resolutionSource
	Err    error
}

// resolutionSource names what a *secretResolutionError failed to read. Its
// string form travels on a failed cross-node dispatch row.
type resolutionSource string

const (
	// resolutionSourceSecrets is the secret backend (the zero value).
	resolutionSourceSecrets resolutionSource = ""
	// resolutionSourceEnvSettings is the hub's stored environment settings
	// (resolveEnvFromStorage: the scope env vars and the progeny list).
	resolutionSourceEnvSettings resolutionSource = "env_settings"
)

func (e *secretResolutionError) Error() string {
	if e.Source == resolutionSourceEnvSettings {
		return "agent environment settings could not be read; the agent was not " + e.Verb
	}
	return "agent secrets could not be resolved; the agent was not " + e.Verb
}

func (e *secretResolutionError) Unwrap() error { return e.Err }

// secretResolutionVerb is the Verb for a dispatch op: "restarted" for a
// restart, "created" for a create or finalize_env, "started" for anything
// else (a start, and a restart's start leg, which dispatches as a start).
func secretResolutionVerb(op string) string {
	switch op {
	case "restart":
		return "restarted"
	case "create", "finalize_env":
		return "created"
	default:
		return "started"
	}
}

// isSecretResolutionError reports whether err is or wraps a
// *secretResolutionError.
func isSecretResolutionError(err error) bool {
	var e *secretResolutionError
	return errors.As(err, &e)
}

// secretResolutionSourceOf returns the Source of the *secretResolutionError
// in err's chain, or resolutionSourceSecrets when there is none.
func secretResolutionSourceOf(err error) resolutionSource {
	var e *secretResolutionError
	if errors.As(err, &e) {
		return e.Source
	}
	return resolutionSourceSecrets
}

// writeSecretResolutionError writes a 503 unavailable response for a
// *secretResolutionError and reports whether it did. The body carries only
// the error's fixed message.
func writeSecretResolutionError(w http.ResponseWriter, err error) bool {
	var e *secretResolutionError
	if !errors.As(err, &e) {
		return false
	}
	writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, e.Error(), nil)
	return true
}

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

package hub

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Hub error codes for a GCP identity "assign" dispatch on Kubernetes that a
// broker refused because of its kubernetes_service_account_mappings
// (ptone/scion#4024). They share their wire value with the broker codes.
const (
	ErrCodeIdentityNotMapped   = api.BrokerErrCodeIdentityNotMapped
	ErrCodeIdentityKSAMismatch = api.BrokerErrCodeIdentityKSAMismatch
)

// kubernetesIdentityMappingDocsURL links the docs section on mapping a GCP
// service account to a Kubernetes ServiceAccount.
const kubernetesIdentityMappingDocsURL = "https://googlecloudplatform.github.io/scion/" +
	"hosted/ha/kubernetes/#gcp-identity-mode-assign-workload-identity-mapping"

// identityMappingError is a broker identity mapping refusal translated into
// the hub's client-facing form.
type identityMappingError struct {
	Code    string
	Message string
	Details map[string]interface{}
}

// identityMappingDispatchError recognises a broker's identity_not_mapped or
// identity_ksa_mismatch refusal in err by its status (400) and error code,
// never by its text, and returns the hub's translation of it. It returns
// false for any other error.
//
// The message names the account, the profile (or runtime entry) and the
// broker, says who can fix it, and links the docs. The broker's own text is
// written for its operator and is not passed on.
func identityMappingDispatchError(err error) (identityMappingError, bool) {
	var se *brokerStatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest {
		return identityMappingError{}, false
	}
	code := se.brokerErrorCode()
	if code != ErrCodeIdentityNotMapped && code != ErrCodeIdentityKSAMismatch {
		return identityMappingError{}, false
	}
	bd := se.brokerErrorDetails()
	detail := func(key string) string {
		v, _ := bd[key].(string)
		return v
	}
	account := detail(api.BrokerErrDetailServiceAccount)
	profile := detail(api.BrokerErrDetailProfile)
	runtimeEntry := detail(api.BrokerErrDetailRuntimeEntry)
	broker := detail(api.BrokerErrDetailBroker)

	accountText := "The GCP service account"
	if account != "" {
		accountText = fmt.Sprintf("GCP service account %q", account)
	}
	scope := identityMappingScopeText(profile, runtimeEntry, broker)

	details := map[string]interface{}{"docs": kubernetesIdentityMappingDocsURL}
	for k, v := range map[string]string{
		api.BrokerErrDetailServiceAccount: account,
		api.BrokerErrDetailProfile:        profile,
		api.BrokerErrDetailRuntimeEntry:   runtimeEntry,
		api.BrokerErrDetailBroker:         broker,
	} {
		if v != "" {
			details[k] = v
		}
	}

	var msg string
	if code == ErrCodeIdentityNotMapped {
		msg = fmt.Sprintf("%s has no Kubernetes service account mapping on %s. "+
			"A broker operator must add it to kubernetes_service_account_mappings in that broker's settings; see %s",
			accountText, scope, kubernetesIdentityMappingDocsURL)
	} else {
		requested := detail(api.BrokerErrDetailRequestedKSA)
		mapped := detail(api.BrokerErrDetailMappedKSA)
		if requested != "" {
			details[api.BrokerErrDetailRequestedKSA] = requested
		}
		if mapped != "" {
			details[api.BrokerErrDetailMappedKSA] = mapped
		}
		requestedText := "The requested Kubernetes service account"
		if requested != "" {
			requestedText = fmt.Sprintf("The requested Kubernetes service account %q", requested)
		}
		mappedText := "the one mapped"
		if mapped != "" {
			mappedText = fmt.Sprintf("%q, the one mapped", mapped)
		}
		msg = fmt.Sprintf("%s does not match %s to %s on %s. "+
			"Remove the explicit Kubernetes service account from the request, or ask a broker operator to change kubernetes_service_account_mappings in that broker's settings; see %s",
			requestedText, mappedText, accountText, scope, kubernetesIdentityMappingDocsURL)
	}
	return identityMappingError{Code: code, Message: msg, Details: details}, true
}

// identityMappingScopeText names where the mapping was looked up: the
// profile when one was selected, else the runtime entry, and the broker.
func identityMappingScopeText(profile, runtimeEntry, broker string) string {
	var where string
	switch {
	case profile != "":
		where = fmt.Sprintf("profile %q", profile)
	case runtimeEntry != "":
		where = fmt.Sprintf("runtime entry %q", runtimeEntry)
	default:
		where = "the default runtime"
	}
	if broker == "" {
		return where + " of the selected broker"
	}
	return fmt.Sprintf("%s of broker %q", where, broker)
}

// relayIdentityMappingError writes a broker identity mapping refusal as an
// HTTP 400 with code identity_not_mapped or identity_ksa_mismatch and the
// hub's message, instead of the generic 502 runtime_error, and reports
// whether it did. For any other error it writes nothing and returns false.
func relayIdentityMappingError(w http.ResponseWriter, err error) bool {
	ime, ok := identityMappingDispatchError(err)
	if !ok {
		return false
	}
	writeError(w, http.StatusBadRequest, ime.Code, ime.Message, ime.Details)
	return true
}

// identityMappingFailureText returns the hub's message for a broker
// identity mapping refusal in err, prefixed with its code, for a failure
// recorded as text (a reincarnation record), and false for any other error.
func identityMappingFailureText(err error) (string, bool) {
	ime, ok := identityMappingDispatchError(err)
	if !ok {
		return "", false
	}
	return ime.Code + ": " + ime.Message, true
}

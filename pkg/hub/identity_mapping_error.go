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
	// se == nil covers a typed nil (*brokerStatusError)(nil) wrapped in a
	// non-nil error, which errors.As accepts.
	if !errors.As(err, &se) || se == nil || se.StatusCode != http.StatusBadRequest {
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
		discovery := detail(api.BrokerErrDetailDiscovery)
		namespace := detail(api.BrokerErrDetailNamespace)
		discoveryText := identityDiscoveryText(discovery, namespace)
		if discoveryText != "" {
			details[api.BrokerErrDetailDiscovery] = discovery
			if namespace != "" {
				details[api.BrokerErrDetailNamespace] = namespace
			}
			discoveryText += " "
		}
		msg = fmt.Sprintf("%s has no Kubernetes service account mapping on %s. %s"+
			"A broker operator must add it to kubernetes_service_account_mappings in that broker's settings; see %s",
			accountText, scope, discoveryText, kubernetesIdentityMappingDocsURL)
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
		discovered := detail(api.BrokerErrDetailKSASource) == api.BrokerKSASourceDiscovered
		oneText, toText, remedy := "the one mapped", "to", "change kubernetes_service_account_mappings in that broker's settings"
		if discovered {
			details[api.BrokerErrDetailKSASource] = api.BrokerKSASourceDiscovered
			oneText = "the one found by its iam.gke.io/gcp-service-account annotation"
			toText = "for"
			remedy = "add an entry to kubernetes_service_account_mappings in that broker's settings"
		}
		mappedText := oneText
		if mapped != "" {
			mappedText = fmt.Sprintf("%q, %s", mapped, oneText)
		}
		msg = fmt.Sprintf("%s does not match %s %s %s on %s. "+
			"Remove the explicit Kubernetes service account from the request, or ask a broker operator to %s; see %s",
			requestedText, mappedText, toText, accountText, scope, remedy, kubernetesIdentityMappingDocsURL)
	}
	return identityMappingError{Code: code, Message: msg, Details: details}, true
}

// identityDiscoveryText is the fixed sentence the hub adds to an
// identity_not_mapped message for the broker's annotation lookup result
// (api.BrokerErrDetailDiscovery), or "" for none or an unknown value. It is
// built from the result value and namespace only; the broker's own text,
// which can carry API server output, is never relayed.
func identityDiscoveryText(result, namespace string) string {
	where := "the agent's namespace"
	if namespace != "" {
		where = fmt.Sprintf("namespace %q", namespace)
	}
	switch result {
	case api.BrokerKSADiscoveryNoMatch:
		return fmt.Sprintf("No Kubernetes service account in %s carries the iam.gke.io/gcp-service-account annotation for it.", where)
	case api.BrokerKSADiscoveryListFailed:
		return fmt.Sprintf("The broker could not list Kubernetes service accounts in %s to find an annotated one; it needs read-only list access to serviceaccounts there.", where)
	case api.BrokerKSADiscoveryUnavailable:
		return "The broker could not look for a Kubernetes service account by its iam.gke.io/gcp-service-account annotation."
	default:
		return ""
	}
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

// dispatchFailureText is the text recorded or warned for a failed broker
// dispatch, wherever the failure is reported as text rather than as an HTTP
// response: a reincarnation record, and the warning of a provision-only
// create. It is the hub's identity_not_mapped or identity_ksa_mismatch
// message for a Kubernetes identity mapping refusal (ptone/scion#4024),
// otherwise err's own text, and "" for a nil err or a typed nil
// *brokerStatusError.
func dispatchFailureText(err error) string {
	if err == nil {
		return ""
	}
	// A typed nil *brokerStatusError, direct or wrapped, is a non-nil error
	// whose Error method would dereference the nil pointer.
	var se *brokerStatusError
	if errors.As(err, &se) && se == nil {
		return ""
	}
	if text, ok := identityMappingFailureText(err); ok {
		return text
	}
	return err.Error()
}

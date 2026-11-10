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

package runtimebroker

import (
	"errors"
	"net/http"

	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// resourceLimitsErrorFrom returns the runtime's refusal of the agent's
// resource spec (scionrt.ResourceLimitsError) carried by err, wrapped or
// not.
func resourceLimitsErrorFrom(err error) (*scionrt.ResourceLimitsError, bool) {
	var re *scionrt.ResourceLimitsError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

// resourceLimitsStatus is the status a start refused for its resource spec
// answers with: the caller must fix the request.
const resourceLimitsStatus = http.StatusBadRequest

// resourceLimitsText is the client text for a start refused for its
// resource spec: "Failed to <op>: " and the error's public message (field
// names, the caller's quoted values and the platform rule; no runtime
// identity), ptone/scion#7103.
func resourceLimitsText(op string, re *scionrt.ResourceLimitsError) string {
	return failedOpText(op) + ": " + re.PublicMessage()
}

// writeResourceLimitsRefused answers a start refused for its resource spec
// with 400 validation_error, which the hub relays to its caller unchanged,
// instead of the fixed runtime_error 500. The caller logs the full error.
func writeResourceLimitsRefused(w http.ResponseWriter, re *scionrt.ResourceLimitsError, op string, details map[string]interface{}) {
	ValidationError(w, resourceLimitsText(op, re), details)
}

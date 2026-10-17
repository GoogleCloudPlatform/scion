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

package conduit

import (
	"strings"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// RefusalHeader marks an RpcResponse as a refusal by the receiving
// session layer, made before any handler ran, as opposed to a handler's
// own answer. Only this package sets it, and it strips it from every
// handler reply, so handler code can never emit it. Its value set is
// closed: RefusalUnsupported on a 501 and RefusalDraining on a 503.
const RefusalHeader = "X-Conduit-Refusal"

// Refusal marker values.
const (
	// RefusalUnsupported: the session does not serve RPCs (501).
	RefusalUnsupported = "unsupported"
	// RefusalDraining: the session is draining after GoAway (503).
	RefusalDraining = "draining"
)

// RefusalOf returns the session-layer refusal resp carries:
// RefusalUnsupported for a 501 marked unsupported, RefusalDraining for a
// 503 marked draining, and "" otherwise. A marker with any other value,
// or on any other status, counts as no marker.
func RefusalOf(resp *conduitv1.RpcResponse) string {
	if resp == nil {
		return ""
	}
	v, ok := headerValueFold(resp.GetHeaders(), RefusalHeader)
	if !ok {
		return ""
	}
	switch {
	case resp.GetStatus() == 501 && v == RefusalUnsupported:
		return RefusalUnsupported
	case resp.GetStatus() == 503 && v == RefusalDraining:
		return RefusalDraining
	}
	return ""
}

// StripRefusal removes every RefusalHeader entry from headers, matching
// the name case-insensitively.
func StripRefusal(headers map[string]string) {
	for k := range headers {
		if strings.EqualFold(k, RefusalHeader) {
			delete(headers, k)
		}
	}
}

// headerValueFold returns the value of the header named name, matching
// case-insensitively.
func headerValueFold(headers map[string]string, name string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

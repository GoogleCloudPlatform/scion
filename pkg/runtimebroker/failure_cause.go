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

import "github.com/GoogleCloudPlatform/scion/pkg/util/logging"

// maxLoggedCauseBytes bounds a sanitized failure cause in a log record.
const maxLoggedCauseBytes = logging.MaxSanitizedTextBytes

// redactedMarker replaces every value sanitizeFailureCause removes.
const redactedMarker = logging.RedactedMarker

// sanitizeFailureCause returns text, a runtime failure's backend error
// text, in the form a broker log record may carry (logging.SanitizeText).
// The records of the create and start failures whose client response is
// the fixed "Failed to <op>" text (synchronous create, async launch, create
// preflight, start, restart, and start context preparation) pass their
// cause and error fields through this. The text never reaches a client
// response.
func sanitizeFailureCause(text string) string {
	return logging.SanitizeText(text)
}

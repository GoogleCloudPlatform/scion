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

package brokercredentials

import (
	"errors"
	"fmt"
	"strings"
)

// ErrBrokerIDNotPathElement is the error of a broker ID that is not a
// single path element (ValidateBrokerIDPathElement).
var ErrBrokerIDNotPathElement = errors.New("broker ID is not a single path element")

// ValidateBrokerIDPathElement reports whether id can be used as one element
// of a file path, as the broker's state directory does
// (<state root>/<broker-id>). It rejects, and never rewrites: the empty
// string, ".", any "..", a path separator ("/" or "\"), and NUL. Broker IDs
// are UUIDs or short names, so no valid ID is rejected.
func ValidateBrokerIDPathElement(id string) error {
	if id == "" || id == "." || strings.Contains(id, "..") || strings.ContainsAny(id, "/\\\x00") {
		return fmt.Errorf("%w: %q", ErrBrokerIDNotPathElement, id)
	}
	return nil
}

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

package labels

import "fmt"

const (
	MaxLabels   = 16
	MaxKeyLen   = 63
	MaxValueLen = 63
)

// Validate checks that labels satisfy all constraints:
//   - At most 16 labels
//   - Keys must not be empty
//   - Keys and values must be printable ASCII (0x20–0x7E)
//   - Keys max 63 characters, values max 63 characters
func Validate(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("too many labels: %d exceeds maximum of %d", len(labels), MaxLabels)
	}
	for k, v := range labels {
		if k == "" {
			return fmt.Errorf("label key must not be empty")
		}
		if len(k) > MaxKeyLen {
			return fmt.Errorf("label key %q exceeds maximum length of %d characters", k, MaxKeyLen)
		}
		if len(v) > MaxValueLen {
			return fmt.Errorf("label value for key %q exceeds maximum length of %d characters", k, MaxValueLen)
		}
		if err := checkPrintableASCII(k); err != nil {
			return fmt.Errorf("label key %q: %w", k, err)
		}
		if err := checkPrintableASCII(v); err != nil {
			return fmt.Errorf("label value for key %q: %w", k, err)
		}
	}
	return nil
}

func checkPrintableASCII(s string) error {
	for i, c := range s {
		if c < 0x20 || c > 0x7E {
			return fmt.Errorf("contains non-printable ASCII character at position %d", i)
		}
	}
	return nil
}

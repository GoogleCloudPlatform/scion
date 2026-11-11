//go:build !unix

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

package cmd

import (
	"errors"
	"fmt"
)

// openFlatCredentialsFile is precondition P6, which is unix-only (errata
// E-2): the Runtime Broker and the server run on unix only. Here it fails
// closed; it is never a weaker path-based check.
func openFlatCredentialsFile(string) ([]byte, error) {
	return nil, fmt.Errorf("checking a cloudrun credentials_file: %w on this platform", errors.ErrUnsupported)
}

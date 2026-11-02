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

package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// The credential state is published and read only inside an agent
// container (sciontool init and doctor), which never runs on this
// platform. Publishing needs directory descriptors, O_NOFOLLOW and owner
// checks this platform does not offer, so every operation here fails with
// errors.ErrUnsupported rather than falling back to an unchecked path.

var credentialStateUnsupportedOnce sync.Once

// setCredentialStateDirForTest does nothing here: nothing is published.
func setCredentialStateDirForTest(dir, base string) func() {
	return func() {}
}

// writeCredentialState always fails with errors.ErrUnsupported.
func writeCredentialState(CredentialState) error {
	return fmt.Errorf("publish credential state: %w on this platform", errors.ErrUnsupported)
}

// PublishCredentialState never publishes on this platform. It logs the
// reason once, at debug level, and returns false.
func PublishCredentialState(ctx context.Context, c *Client) bool {
	credentialStateUnsupportedOnce.Do(func() {
		log.Debug("Not publishing the credential state: %v", writeCredentialState(CredentialState{}))
	})
	return false
}

// ReadCredentialState reports the state as unavailable: nothing publishes
// it on this platform.
func ReadCredentialState() (st CredentialState, status, reason string) {
	return st, CredentialStateUnavailable, "not supported on this platform"
}

// ReadTokenFileWithModTime reports no usable value: the guarded token-file
// read is not supported on this platform (see readTokenAtStat).
func ReadTokenFileWithModTime() (token string, modTime time.Time, ok bool) {
	return "", time.Time{}, false
}

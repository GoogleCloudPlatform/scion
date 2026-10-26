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

package runtimebroker

import (
	"errors"
	"fmt"
	"os"
)

// lockJournalInstance always fails with errors.ErrUnsupported. The unix
// version takes a non-blocking flock on <root>/<id>.lock. A Windows
// byte-range lock is mandatory, not advisory, so it is not a drop-in
// replacement; the launch journal is used only by a Runtime Broker, which
// is not supported on this platform. It creates nothing.
func lockJournalInstance(root, instanceID string, mustExist bool) (*os.File, error) {
	return nil, fmt.Errorf("lock launch journal instance %s: %w on this platform", instanceID, errors.ErrUnsupported)
}

// unlockJournalInstance closes f; lockJournalInstance never returns a
// locked file on this platform.
func unlockJournalInstance(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

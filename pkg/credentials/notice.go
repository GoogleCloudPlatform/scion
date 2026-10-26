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

package credentials

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// The read paths (Load, and through it GetAccessToken and IsAuthenticated)
// turn a refused credentials file into "not logged in", which callers act
// on without showing why. So the refusal itself, with its fix hint, is
// printed here: on stderr, never stdout (JSON and other machine-read output
// stay clean), and once per process. Only a privacy refusal is printed
// (privatefs.ErrNotPrivate, Windows only: the unix load does no privacy
// check); other load errors keep their silent behaviour.
// Both variables are guarded by mu.
var (
	// noticeOut receives the notices; tests replace it.
	noticeOut io.Writer = os.Stderr
	// refusalReported is set once the read-side refusal has been printed.
	// It is atomic so the notice stays once-only even without mu.
	refusalReported atomic.Bool
)

// reportRefusal prints a privacy refusal from a read path once per process.
// The caller holds mu (for noticeOut); the once-only check does not depend
// on it.
func reportRefusal(err error) {
	if !errors.Is(err, privatefs.ErrNotPrivate) || !refusalReported.CompareAndSwap(false, true) {
		return
	}
	_, _ = fmt.Fprintf(noticeOut, "Warning: %v\n", err)
}

// warnReplacingRefused tells the user that a login is replacing a refused
// credentials file, whose other logins are lost (its contents are never
// read). The caller holds mu.
func warnReplacingRefused(path string, err error) {
	if !errors.Is(err, privatefs.ErrNotPrivate) {
		return
	}
	_, _ = fmt.Fprintf(noticeOut, "Warning: the credentials file %s is not private, so it was not read. It is being replaced by a private file holding only this login; logins to other hubs stored in it must be repeated with 'scion hub auth login'.\n", path)
}

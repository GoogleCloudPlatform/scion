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

//go:build !unix

package telemetry

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func verifyAgyParentDir(dir string, _ *OutboxOwner) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("antigravity parent dir %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("antigravity parent dir %s is not a directory", dir)
	}
	return nil
}

func startAgySQLiteWakeListener(_ string, _ *OutboxOwner, _ func()) (io.Closer, error) {
	return nil, fmt.Errorf("antigravity sqlite wake listener: %w on this platform", errors.ErrUnsupported)
}

// NotifyAgySQLiteWake is a no-op on non-Unix platforms.
func NotifyAgySQLiteWake(_ string) {}

// NotifyAgySQLiteWakePath is a no-op on non-Unix platforms.
func NotifyAgySQLiteWakePath(_ string) {}

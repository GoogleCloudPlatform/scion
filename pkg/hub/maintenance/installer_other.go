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

package maintenance

import (
	"errors"
	"fmt"
	"os"
)

func signalSelf() error {
	return fmt.Errorf("restarting the hub: %w on this platform", errors.ErrUnsupported)
}

func fileOwnerUID(os.FileInfo) (int, bool) { return 0, false }

func isWritableDir(string) bool { return false }

// The symlink installer (installer_symlink.go) is unix-only. Detect returns
// the unsupported installer before it would construct one off Linux, and
// the sudo-install strategy is never selected here either; these stubs
// only keep the cross-platform callers compiling.

// symlinkInstaller off unix is an unsupported installer.
type symlinkInstaller struct{ unsupportedInstaller }

func newSymlinkInstaller(DetectOptions) *symlinkInstaller {
	return &symlinkInstaller{unsupportedInstaller{status: InstallerStatus{
		Strategy: StrategyUnsupported,
		Reason:   "the symlink installer is only available on unix",
	}}}
}

func copyExecutable(string, string) error {
	return fmt.Errorf("installing a binary: %w on this platform", errors.ErrUnsupported)
}

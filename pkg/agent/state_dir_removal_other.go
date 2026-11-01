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

package agent

// retryRemovalInUserNamespace returns removeErr unchanged: the podman
// unshare retry exists only for rootless podman on unix (see
// state_dir_removal_unix.go). A failed removal, including one wrapping
// errors.ErrUnsupported, is reported to the caller.
func retryRemovalInUserNamespace(path string, removeErr error) error {
	return removeErr
}

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

package runtime

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// cloudRunHubTokenRel is the agent's token file below its broker-side home,
// where pkg/agent writes the hub-issued credential for every start
// (writeAgentTokenFile). Docker bind-mounts that home and Kubernetes copies
// it into the pod; a Cloud Run instance mounts its own home leaf from the
// NFS share instead, so the credential is carried to it in the staged
// secrets (Staged.HubToken) and sciontool init writes it back to the same
// relative path in the instance's home.
const cloudRunHubTokenRel = ".scion/scion-token"

// cloudRunHubTokenMaxBytes caps the token read, matching sciontool's own
// token file reader.
const cloudRunHubTokenMaxBytes = 8192

// Conditions a refused token read is reported with. They name what is wrong
// and never carry the file's content.
const (
	hubTokenCondHome     = "agent home is not an allowed home path"
	hubTokenCondLinked   = "token file is a link or not a single-link regular file"
	hubTokenCondTooLarge = "token file exceeds the size limit"
	hubTokenCondRead     = "token file is unreadable"
)

// readCloudRunHubToken returns the hub-issued credential pkg/agent wrote
// under homeDir, for staging into the Cloud Run instance. It returns "" with
// an empty condition when there is no home or no token file (the start has
// no hub credential). A file that exists but is refused returns "" and the
// condition: every directory below the validated home and the file itself
// are opened without following symbolic links, and the file must be a
// single-link regular file within cloudRunHubTokenMaxBytes, so a link
// planted at the token path never makes the broker read and ship another
// file's content.
func readCloudRunHubToken(homeDir string) (token, condition string) {
	if homeDir == "" {
		return "", ""
	}
	home, err := ValidateAgentHomeSource(homeDir)
	if err != nil || home == "" {
		return "", hubTokenCondHome
	}
	dirFd, leaf, err := dirfd.OpenParentNoFollowUnder(home, cloudRunHubTokenRel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ""
		}
		return "", classifyHubTokenReadErr(err)
	}
	defer func() { _ = dirfd.Close(dirFd) }()
	data, _, err := dirfd.ReadAtNoFollowStat(dirFd, leaf, cloudRunHubTokenMaxBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ""
		}
		return "", classifyHubTokenReadErr(err)
	}
	return strings.TrimSpace(string(data)), ""
}

func classifyHubTokenReadErr(err error) string {
	switch {
	case errors.Is(err, dirfd.ErrTooLarge):
		return hubTokenCondTooLarge
	case errors.Is(err, dirfd.ErrNotSingleLinkRegular), errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return hubTokenCondLinked
	default:
		return hubTokenCondRead
	}
}

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
	"os"
	"os/exec"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// isRuntimeBinaryNotFound reports whether err indicates that the container
// runtime CLI binary itself could not be found or executed (as opposed to the
// binary running and exiting non-zero).
func isRuntimeBinaryNotFound(err error) bool {
	if err == nil || isExitError(err) {
		return false
	}
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// isInvalidImageReference reports whether image is empty or contains
// whitespace, which can never be a valid container image reference.
func isInvalidImageReference(image string) bool {
	return strings.TrimSpace(image) == "" || strings.ContainsAny(image, " \t\r\n")
}

// isInvalidImageOutput reports whether container CLI/daemon output indicates a
// malformed image reference across Docker, Podman, and Apple Container.
func isInvalidImageOutput(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "invalid reference format") ||
		strings.Contains(lower, "repository name must be lowercase") ||
		strings.Contains(lower, "invalid tag format") ||
		strings.Contains(lower, "invalid digest format") ||
		strings.Contains(lower, "repository name must have at least one component") ||
		strings.Contains(lower, "repository name must not be more than") ||
		strings.Contains(lower, "repository name must be canonical") ||
		strings.Contains(lower, "not a valid repository/tag") ||
		strings.Contains(lower, "unable to parse reference") ||
		strings.Contains(lower, "failed to parse image reference") ||
		strings.Contains(lower, "failed to parse reference") ||
		strings.Contains(lower, "invalid image reference") ||
		strings.Contains(lower, "invalid reference:") ||
		strings.Contains(lower, "malformed reference")
}

// isDockerDaemonConnectError reports whether Docker CLI output indicates a
// failure to reach or authenticate to the Docker daemon socket/API itself,
// rather than an image or registry error returned by a reachable daemon.
func isDockerDaemonConnectError(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "cannot connect to the docker daemon") ||
		strings.Contains(lower, "is the docker daemon running") ||
		strings.Contains(lower, "permission denied while trying to connect to the docker") ||
		strings.Contains(lower, "error during connect:")
}

// isDockerRunImagePullOutput reports whether `docker run` output indicates
// that an implicit image pull failed (as opposed to a mount, flag, or
// container start error).
func isDockerRunImagePullOutput(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "unable to find image ") ||
		strings.Contains(lower, "manifest unknown") ||
		strings.Contains(lower, "pull access denied") ||
		strings.Contains(lower, "no matching manifest for ")
}

func classifyDockerPullError(image, out string, err error) string {
	if err == nil {
		return ""
	}
	if isRuntimeBinaryNotFound(err) {
		return api.LaunchCodeRuntimeUnavailable
	}
	if !isExitError(err) {
		return ""
	}
	if isDockerDaemonConnectError(out) {
		return api.LaunchCodeRuntimeUnavailable
	}
	if isInvalidImageReference(image) || isInvalidImageOutput(out) {
		return api.LaunchCodeInvalidImage
	}
	return api.LaunchCodeImagePullFailed
}

func classifyDockerRunError(out string, err error) (code, step string) {
	if err == nil {
		return "", ""
	}
	if isRuntimeBinaryNotFound(err) {
		return api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching
	}
	if !isExitError(err) {
		return "", ""
	}
	if isDockerDaemonConnectError(out) {
		return api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching
	}
	if isInvalidImageOutput(out) {
		return api.LaunchCodeInvalidImage, api.LaunchStepImagePull
	}
	if isDockerRunImagePullOutput(out) {
		return api.LaunchCodeImagePullFailed, api.LaunchStepImagePull
	}
	return "", ""
}

// isPodmanDaemonConnectError reports whether Podman CLI output indicates that
// the Podman machine VM or remote Podman service socket is not running or
// cannot be reached.
func isPodmanDaemonConnectError(output string) bool {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "cannot connect to podman") ||
		strings.Contains(lower, "unable to connect to podman") ||
		strings.Contains(lower, "failed to connect to podman") ||
		strings.Contains(lower, "is the podman machine running") ||
		strings.Contains(lower, "podman machine start") ||
		strings.Contains(lower, "podman machine init") ||
		strings.Contains(lower, "connection to the podman service") {
		return true
	}
	if strings.Contains(lower, "dial unix") &&
		(strings.Contains(lower, "connection refused") ||
			strings.Contains(lower, "no such file or directory") ||
			strings.Contains(lower, "permission denied")) {
		return true
	}
	return false
}

// isPodmanRunImagePullOutput reports whether `podman run` output indicates
// that an implicit image pull failed (as opposed to a mount, flag, or
// container start error).
func isPodmanRunImagePullOutput(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "trying to pull ") ||
		strings.Contains(lower, "initializing source ") ||
		strings.Contains(lower, "unable to copy from source") ||
		strings.Contains(lower, "error pinging docker registry") ||
		strings.Contains(lower, "short-name resolution enforced") ||
		strings.Contains(lower, "manifest unknown") ||
		strings.Contains(lower, "pull access denied") ||
		strings.Contains(lower, "no matching manifest for ")
}

func classifyPodmanPullError(image, out string, err error) string {
	if err == nil {
		return ""
	}
	if isRuntimeBinaryNotFound(err) {
		return api.LaunchCodeRuntimeUnavailable
	}
	if !isExitError(err) {
		return ""
	}
	if isPodmanDaemonConnectError(out) {
		return api.LaunchCodeRuntimeUnavailable
	}
	if isInvalidImageReference(image) || isInvalidImageOutput(out) {
		return api.LaunchCodeInvalidImage
	}
	return api.LaunchCodeImagePullFailed
}

func classifyPodmanRunError(out string, err error) (code, step string) {
	if err == nil {
		return "", ""
	}
	if isRuntimeBinaryNotFound(err) {
		return api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching
	}
	if !isExitError(err) {
		return "", ""
	}
	if isPodmanDaemonConnectError(out) {
		return api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching
	}
	if isInvalidImageOutput(out) {
		return api.LaunchCodeInvalidImage, api.LaunchStepImagePull
	}
	if isPodmanRunImagePullOutput(out) {
		return api.LaunchCodeImagePullFailed, api.LaunchStepImagePull
	}
	return "", ""
}

// isAppleContainerDaemonConnectError reports whether Apple `container` CLI
// output indicates that the Apple container system service (apiserver / XPC)
// is not running or cannot be reached.
func isAppleContainerDaemonConnectError(output string) bool {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "container system start") ||
		strings.Contains(lower, "unable to connect to container") ||
		strings.Contains(lower, "cannot connect to container") ||
		strings.Contains(lower, "container service is not running") ||
		strings.Contains(lower, "container system is not running") ||
		strings.Contains(lower, "system service is not running") ||
		strings.Contains(lower, "daemon is not running") ||
		strings.Contains(lower, "com.apple.container.apiserver") ||
		strings.Contains(lower, "xpc connection") ||
		strings.Contains(lower, "connection invalid") {
		return true
	}
	if strings.Contains(lower, "apiserver") &&
		(strings.Contains(lower, "not running") ||
			strings.Contains(lower, "unavailable") ||
			strings.Contains(lower, "connection") ||
			strings.Contains(lower, "failed to connect")) {
		return true
	}
	return false
}

// isAppleContainerRunImagePullOutput reports whether Apple `container run`
// output indicates that an implicit image pull failed.
func isAppleContainerRunImagePullOutput(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "failed to pull image") ||
		strings.Contains(lower, "failed to fetch") ||
		strings.Contains(lower, "failed to resolve image") ||
		strings.Contains(lower, "manifest unknown") ||
		strings.Contains(lower, "manifest not found") ||
		strings.Contains(lower, "image not found") ||
		strings.Contains(lower, "pull access denied") ||
		strings.Contains(lower, "not found in registry") ||
		strings.Contains(lower, "no matching manifest for ")
}

func classifyAppleContainerPullError(image, out string, err error) string {
	if err == nil {
		return ""
	}
	if isRuntimeBinaryNotFound(err) {
		return api.LaunchCodeRuntimeUnavailable
	}
	if !isExitError(err) {
		return ""
	}
	if isAppleContainerDaemonConnectError(out) {
		return api.LaunchCodeRuntimeUnavailable
	}
	if isInvalidImageReference(image) || isInvalidImageOutput(out) {
		return api.LaunchCodeInvalidImage
	}
	return api.LaunchCodeImagePullFailed
}

func classifyAppleContainerRunError(out string, err error) (code, step string) {
	if err == nil {
		return "", ""
	}
	if isRuntimeBinaryNotFound(err) {
		return api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching
	}
	if !isExitError(err) {
		return "", ""
	}
	if isAppleContainerDaemonConnectError(out) {
		return api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching
	}
	if isInvalidImageOutput(out) {
		return api.LaunchCodeInvalidImage, api.LaunchStepImagePull
	}
	if isAppleContainerRunImagePullOutput(out) {
		return api.LaunchCodeImagePullFailed, api.LaunchStepImagePull
	}
	return "", ""
}

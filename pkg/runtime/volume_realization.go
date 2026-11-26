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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// VolumeRealization says how a runtime realizes an api.VolumeMount.
type VolumeRealization int

const (
	// RealizeHostBind means the volume's source is a broker host path;
	// brokercontrol and configured-volume guards apply.
	RealizeHostBind VolumeRealization = iota
	// RealizeNamed means the volume's source is a container-daemon named
	// volume (no path separator, matches isNamedVolume).
	RealizeNamed
	// RealizeFUSE means a GCS volume mounted via gcsfuse inside the
	// container (docker, podman, apple-container).
	RealizeFUSE
	// RealizeCSI means a GCS volume mounted via the GKE GCS FUSE CSI
	// driver (kubernetes).
	RealizeCSI
	// RealizeUnsupported means the runtime cannot realize this volume type
	// and must refuse it (on bind runtimes) or skip it (on runtimes that
	// do not realize configured volumes of that type).
	RealizeUnsupported
)

// String returns a human-readable name for r.
func (r VolumeRealization) String() string {
	switch r {
	case RealizeHostBind:
		return "host_bind"
	case RealizeNamed:
		return "named"
	case RealizeFUSE:
		return "fuse"
	case RealizeCSI:
		return "csi"
	case RealizeUnsupported:
		return "unsupported"
	default:
		return fmt.Sprintf("VolumeRealization(%d)", int(r))
	}
}

// ErrUnsupportedVolumeType is wrapped by UnsupportedVolumeTypeError when a
// configured volume type cannot be realized by the target runtime.
var ErrUnsupportedVolumeType = errors.New("unsupported volume type")

// UnsupportedVolumeTypeError is returned when a volume's type cannot be
// realized by the target runtime (for example cloudrun-volume or
// gke-shared-volume on a bind runtime such as docker, podman, or container).
// Its message names only the volume type, target and runtime, never a host path.
type UnsupportedVolumeTypeError struct {
	Index   int // -1 when not tied to a slice index
	Type    string
	Target  string
	Runtime string
}

func (e *UnsupportedVolumeTypeError) Error() string {
	rt := e.Runtime
	if rt == "" {
		rt = "bind runtime"
	}
	if e.Index >= 0 {
		return fmt.Sprintf("volumes[%d] (target %s): %s %q is not supported by runtime %q",
			e.Index, e.Target, ErrUnsupportedVolumeType, e.Type, rt)
	}
	return fmt.Sprintf("volume (target %s): %s %q is not supported by runtime %q",
		e.Target, ErrUnsupportedVolumeType, e.Type, rt)
}

func (e *UnsupportedVolumeTypeError) Unwrap() error {
	return ErrUnsupportedVolumeType
}

// PublicMessage returns a safe client-facing description naming only the
// volume type, target and runtime (never a host path).
func (e *UnsupportedVolumeTypeError) PublicMessage() string {
	return e.Error()
}

func unsupportedVolumeTypeError(index int, v api.VolumeMount, runtimeKind string) *UnsupportedVolumeTypeError {
	return &UnsupportedVolumeTypeError{
		Index:   index,
		Type:    strings.TrimSpace(v.Type),
		Target:  v.Target,
		Runtime: strings.TrimSpace(runtimeKind),
	}
}

// isKubernetesRuntimeKind reports whether rt (lowercased, trimmed) is a
// recognized Kubernetes runtime spelling, including the legacy "k8s" and
// "remote" aliases.
func isKubernetesRuntimeKind(rt string) bool {
	switch rt {
	case "kubernetes", "k8s", "remote":
		return true
	default:
		return false
	}
}

// isCloudRunOrSubstrateRuntimeKind reports whether rt (lowercased, trimmed)
// is a Cloud Run or Substrate runtime kind.
func isCloudRunOrSubstrateRuntimeKind(rt string) bool {
	switch rt {
	case "cloudrun", "cloudrun-instances", "cloudrun-sandbox", "substrate":
		return true
	default:
		return false
	}
}

// isNonBindRuntime reports whether runtimeKind belongs to the non-bind runtime
// families (kubernetes, cloudrun, substrate) that do not realize configured
// host-bind volumes. Derived from the same family helpers RealizeVolume
// switches on so the two cannot drift.
func isNonBindRuntime(runtimeKind string) bool {
	rt := strings.ToLower(strings.TrimSpace(runtimeKind))
	return isKubernetesRuntimeKind(rt) || isCloudRunOrSubstrateRuntimeKind(rt)
}

// RealizeVolume returns how runtimeKind realizes v.
//
// Volume type matching is case-insensitive and whitespace-trimmed everywhere:
// "" and "local" mean a host bind (or a daemon named volume when Source is a
// valid named volume).
//
// On bind runtimes (docker, podman, container, apple-container, or any
// non-k8s/cloudrun/substrate kind such as "", "local", "auto"):
//   - "", "local" → RealizeNamed (if isNamedVolume(v.Source)) or RealizeHostBind
//   - "nfs" → RealizeHostBind (host path to an NFS mount)
//   - "gcs" → RealizeFUSE (gcsfuse inside the container)
//   - "cloudrun-volume", "gke-shared-volume", and unknown types → RealizeUnsupported
//
// On kubernetes ("kubernetes", "k8s", "remote"):
//   - "gcs" → RealizeCSI
//   - all other types → RealizeUnsupported
//
// On cloudrun (cloudrun, cloudrun-instances, cloudrun-sandbox) and substrate:
//   - all configured volume types → RealizeUnsupported
func RealizeVolume(v api.VolumeMount, runtimeKind string) VolumeRealization {
	volType := strings.ToLower(strings.TrimSpace(v.Type))
	rt := strings.ToLower(strings.TrimSpace(runtimeKind))

	switch {
	case isKubernetesRuntimeKind(rt):
		switch volType {
		case "gcs":
			return RealizeCSI
		default:
			return RealizeUnsupported
		}
	case isCloudRunOrSubstrateRuntimeKind(rt):
		return RealizeUnsupported
	default:
		switch volType {
		case "", "local":
			if isNamedVolume(v.Source) {
				return RealizeNamed
			}
			return RealizeHostBind
		case "nfs":
			return RealizeHostBind
		case "gcs":
			return RealizeFUSE
		default:
			// Includes cloudrun-volume, gke-shared-volume, and unknown types.
			return RealizeUnsupported
		}
	}
}

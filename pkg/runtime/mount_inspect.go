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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
)

// LabelBindSources records, at container creation, the host-path binds the
// broker passed to the runtime (base64 of a JSON []BindMount, sources
// resolved). The broker sets it on the create command line, after every
// other label, and no other label source may use the key (buildCommonRunArgs
// drops one); container labels cannot be changed after creation, and the
// workload has no access to the runtime's API.
const LabelBindSources = "scion.bind_sources"

// BindMount is one host-path bind of a container.
type BindMount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// ErrMountsUnverifiable is returned when a container's binds cannot be
// determined, or cannot be matched against what the broker requested.
// Callers treat it as unsafe.
var ErrMountsUnverifiable = errors.New("container mounts could not be verified")

// errBindMismatch is a container whose binds, as the runtime reports them,
// differ from the binds the broker requested.
var errBindMismatch = errors.New("container mounts differ from the mounts requested")

// errNoBindRecord is a container without any LabelBindSources record: one
// created before the record existed. It wraps ErrMountsUnverifiable.
var errNoBindRecord = fmt.Errorf("%w: no %s label", ErrMountsUnverifiable, LabelBindSources)

// InspectedBinds is what can be learned about an existing container's binds.
type InspectedBinds struct {
	// Reported are the binds as the runtime reports them, or nil when the
	// runtime cannot report them (ReportedKnown false).
	Reported      []BindMount
	ReportedKnown bool
	// Labels are the container's labels, for its LabelBindSources record.
	Labels map[string]string
	// DaemonLocal, when set, reports whether the runtime's daemon is
	// reached over a local unix socket (dockerDaemonLocal,
	// podmanDaemonLocal). It is consulted only for a container without a
	// record (VerifyRunningContainerBinds).
	DaemonLocal func() (bool, error)
}

// MountInspector is implemented by runtimes that can describe the
// host-path binds of a container they created. Runtimes that bind no host
// paths of the broker host (Kubernetes, Cloud Run) do not implement it.
type MountInspector interface {
	InspectBindMounts(ctx context.Context, id string) (InspectedBinds, error)
}

// VerifyContainerBinds decides whether an existing container's binds are
// clear of the Runtime Broker control paths, failing closed:
//   - The container must carry a well-formed LabelBindSources record of
//     what the broker requested; a container without one (created before
//     the record existed, or not by this broker) is unverifiable.
//   - Every recorded source must pass the control-path check against the
//     control paths as they resolve now.
//   - Where the runtime reports the binds itself, they must match the
//     record exactly, by destination: a source that differs, a bind that was
//     not requested and a requested bind that is missing are all refused.
//     A reported source that does not exist on this host can therefore only
//     pass if it is exactly the source the broker resolved and requested.
//     Every reported source is also checked.
//
// A refusal wraps brokercontrol.ErrControlPathMount; an unverifiable
// container wraps ErrMountsUnverifiable.
func VerifyContainerBinds(ib InspectedBinds) error {
	recorded, err := decodeBindSourcesLabel(ib.Labels)
	if err != nil {
		return err
	}
	if err := CheckBindMounts(recorded); err != nil {
		return err
	}
	if !ib.ReportedKnown {
		return nil
	}
	if err := CheckBindMounts(ib.Reported); err != nil {
		return err
	}
	return matchBinds(recorded, ib.Reported)
}

// VerifyRunningContainerBinds is VerifyContainerBinds for a container that
// was already running, which may predate the LabelBindSources record. A
// container with a record is verified exactly as VerifyContainerBinds does.
// A container with no record at all is kept only when all of these hold,
// and is otherwise unverifiable or refused (fail closed):
//   - the runtime reports its binds itself (Docker, Podman; never Apple);
//   - the runtime's daemon is, authoritatively, reached over a local unix
//     socket (InspectedBinds.DaemonLocal; any error counts as not local);
//   - every reported source is absolute, exists on this host and passes the
//     control-path check (checkBindSource).
//
// A reported source with Docker Desktop's host prefix is judged by the host
// path after the prefix (a documented residual for these containers only).
// A malformed or empty record is not "no record": it stays unverifiable.
func VerifyRunningContainerBinds(ib InspectedBinds) error {
	err := VerifyContainerBinds(ib)
	if err == nil || !errors.Is(err, errNoBindRecord) {
		return err
	}
	return verifyUnrecordedBinds(ib)
}

// verifyUnrecordedBinds applies the rules for a container without a record
// (see VerifyRunningContainerBinds).
func verifyUnrecordedBinds(ib InspectedBinds) error {
	if !ib.ReportedKnown {
		return fmt.Errorf("%w: the runtime cannot report the binds of a container without a record", errNoBindRecord)
	}
	if ib.DaemonLocal == nil {
		return fmt.Errorf("%w: the daemon's locality is unknown", errNoBindRecord)
	}
	local, err := ib.DaemonLocal()
	if err != nil {
		return fmt.Errorf("%w: %v", errNoBindRecord, err)
	}
	if !local {
		return fmt.Errorf("%w: the container daemon is not reached over a local unix socket", errNoBindRecord)
	}
	for _, b := range ib.Reported {
		raw := filepath.Clean(b.Source)
		if !filepath.IsAbs(raw) {
			return fmt.Errorf("%w: the bind at %s has a source that is not absolute", errNoBindRecord, b.Destination)
		}
		src := raw
		if strings.HasPrefix(raw, dockerDesktopHostPrefix+"/") {
			src = strings.TrimPrefix(raw, dockerDesktopHostPrefix)
			// Unless the prefix directory is known not to exist on this
			// host (a Docker Desktop host), the prefixed path is checked
			// too, and either check refusing refuses the bind. A prefix
			// directory that exists or cannot be inspected keeps the check,
			// which then fails closed on anything it cannot resolve.
			if _, err := os.Lstat(dockerDesktopHostPrefix); !errors.Is(err, fs.ErrNotExist) {
				if _, err := checkBindSource(raw); err != nil {
					return fmt.Errorf("container mount at %s: %w", b.Destination, err)
				}
			}
		}
		// The control-path check runs first, so a source reaching a control
		// path is refused at once even when it does not exist yet.
		if _, err := checkBindSource(src); err != nil {
			return fmt.Errorf("container mount at %s: %w", b.Destination, err)
		}
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("%w: the source of the bind at %s cannot be found on this host: %v", errNoBindRecord, b.Destination, err)
		}
	}
	return nil
}

// matchBinds compares requested and reported binds by destination.
func matchBinds(recorded, reported []BindMount) error {
	want := map[string]string{}
	for _, b := range recorded {
		want[cleanMountPath(b.Destination)] = filepath.Clean(b.Source)
	}
	seen := map[string]bool{}
	for _, b := range reported {
		dst := cleanMountPath(b.Destination)
		src, ok := want[dst]
		if !ok {
			return fmt.Errorf("%w: %w: a bind at %s was not requested", brokercontrol.ErrControlPathMount, errBindMismatch, b.Destination)
		}
		if !sameReportedSource(b.Source, src) {
			return fmt.Errorf("%w: %w: the bind at %s has another source than requested", brokercontrol.ErrControlPathMount, errBindMismatch, b.Destination)
		}
		seen[dst] = true
	}
	var missing []string
	for dst := range want {
		if !seen[dst] {
			missing = append(missing, dst)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w: %w: requested binds are missing at %s", brokercontrol.ErrControlPathMount, errBindMismatch, strings.Join(missing, ", "))
	}
	return nil
}

// cleanMountPath cleans a container-side path (always slash-separated).
func cleanMountPath(p string) string {
	if p == "" {
		return p
	}
	c := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	return c
}

// dockerDesktopHostPrefix is the prefix Docker Desktop may show in front of a
// host path it shares into its VM. It is the same host path, so it is
// accepted as a match, and nothing else is. A variable only so tests can
// place it in a temporary directory.
var dockerDesktopHostPrefix = "/host_mnt"

// sameReportedSource reports whether a runtime-reported bind source is the
// requested (resolved) source.
func sameReportedSource(reported, requested string) bool {
	r := filepath.Clean(reported)
	if r == requested {
		return true
	}
	return strings.HasPrefix(r, dockerDesktopHostPrefix+"/") && strings.TrimPrefix(r, dockerDesktopHostPrefix) == requested
}

// CheckBindMounts applies the Runtime Broker control-path check
// (checkBindSource) to the sources of binds. It returns the first refusal.
func CheckBindMounts(binds []BindMount) error {
	for _, b := range binds {
		if _, err := checkBindSource(b.Source); err != nil {
			return fmt.Errorf("container mount at %s: %w", b.Destination, err)
		}
	}
	return nil
}

// inspectContainerBinds reads the bind mounts and labels of a Docker or
// Podman container from one "inspect", parsing standard output only
// (runStdoutCommand). Only binds are reported: named
// volumes and tmpfs mounts are the runtime's own storage, not broker-host
// paths.
func inspectContainerBinds(ctx context.Context, command, id string) (InspectedBinds, error) {
	out, err := runStdoutCommand(ctx, command, "inspect", "--format", `{"Mounts":{{json .Mounts}},"Labels":{{json .Config.Labels}}}`, id)
	if err != nil {
		return InspectedBinds{}, fmt.Errorf("%w: inspect: %v", ErrMountsUnverifiable, err)
	}
	return parseInspect([]byte(out))
}

func parseInspect(out []byte) (InspectedBinds, error) {
	var doc struct {
		Mounts []struct {
			Type        string `json:"Type"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		} `json:"Mounts"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return InspectedBinds{}, fmt.Errorf("%w: parse: %v", ErrMountsUnverifiable, err)
	}
	ib := InspectedBinds{ReportedKnown: true, Labels: doc.Labels}
	for _, m := range doc.Mounts {
		if m.Type == "bind" {
			ib.Reported = append(ib.Reported, BindMount{Source: m.Source, Destination: m.Destination})
		}
	}
	return ib, nil
}

// encodeBindSourcesLabel is the LabelBindSources value for binds.
func encodeBindSourcesLabel(binds []BindMount) (string, error) {
	data, err := json.Marshal(binds)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// decodeBindSourcesLabel parses a LabelBindSources value. A missing or
// malformed label is ErrMountsUnverifiable.
func decodeBindSourcesLabel(labels map[string]string) ([]BindMount, error) {
	v, ok := labels[LabelBindSources]
	if !ok {
		return nil, errNoBindRecord
	}
	data, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %s label: %v", ErrMountsUnverifiable, LabelBindSources, err)
	}
	var binds []BindMount
	if err := json.Unmarshal(data, &binds); err != nil {
		return nil, fmt.Errorf("%w: %s label: %v", ErrMountsUnverifiable, LabelBindSources, err)
	}
	return binds, nil
}

// InspectBindMounts reports the container's binds and labels from docker
// inspect.
func (r *DockerRuntime) InspectBindMounts(ctx context.Context, id string) (InspectedBinds, error) {
	ib, err := inspectContainerBinds(ctx, r.Command, id)
	ib.DaemonLocal = func() (bool, error) { return dockerDaemonLocal(ctx, r.Command) }
	return ib, err
}

// InspectBindMounts reports the container's binds and labels from podman
// inspect.
func (r *PodmanRuntime) InspectBindMounts(ctx context.Context, id string) (InspectedBinds, error) {
	ib, err := inspectContainerBinds(ctx, r.Command, id)
	ib.DaemonLocal = func() (bool, error) { return podmanDaemonLocal(ctx, r.Command) }
	return ib, err
}

// InspectBindMounts reports only the container's labels: the Apple
// container runtime's own inspect output is not parsed yet, so
// VerifyContainerBinds checks the broker's LabelBindSources record of what
// it asked for, not the runtime's view of what it mounted.
func (r *AppleContainerRuntime) InspectBindMounts(ctx context.Context, id string) (InspectedBinds, error) {
	ctx, cancel := context.WithTimeout(ctx, stdoutCommandTimeout)
	defer cancel()
	agents, err := r.List(ctx, nil)
	if err != nil {
		return InspectedBinds{}, fmt.Errorf("%w: list: %v", ErrMountsUnverifiable, err)
	}
	for _, a := range agents {
		if a.ContainerID == id || a.Name == id {
			return InspectedBinds{Labels: a.Labels}, nil
		}
	}
	return InspectedBinds{}, fmt.Errorf("%w: container %s not found", ErrMountsUnverifiable, id)
}

/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// The harness provision status file records the outcome of the most recent
// `sciontool harness provision` run, so a pre-start failure of the
// 20-harness-provision hook can be reported with its reason instead of a
// bare "exit status 1". It lives in the bundle's outputs
// directory (outputs/status.json, the manifest's outputs.status), mapped by
// SCION_HARNESS_OUTPUTS_DIR when that is set.
//
// Only `sciontool harness provision` writes it, and only with an error it has
// already scrubbed of staged secret values and truncated. The provisioner
// script's raw stderr is never written here.
const (
	ProvisionStatusRunning = "running"
	ProvisionStatusOK      = "ok"
	ProvisionStatusFailed  = "failed"

	// provisionStatusMaxError bounds the error stored in the status file
	// and the detail surfaced from it, in bytes.
	provisionStatusMaxError = 1024
	// provisionStatusMaxFile bounds how much of the status file is read.
	provisionStatusMaxFile = 16 << 10
)

type provisionStatus struct {
	SchemaVersion int    `json:"schema_version"`
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
}

// HarnessProvisionStatusPath returns the status file path for the harness
// bundle at bundleDir ($HOME/.scion/harness).
func HarnessProvisionStatusPath(bundleDir string) (string, error) {
	dirs, err := ResolveHarnessDirs(bundleDir)
	if err != nil {
		return "", err
	}
	return dirs.OutputPath(filepath.Join(bundleDir, "outputs", "status.json")), nil
}

// WriteHarnessProvisionStatus writes state, and for ProvisionStatusFailed the
// already-scrubbed errMsg (sanitized and truncated again here), to path. The
// write is atomic and never follows a symlink at the leaf or in any parent
// directory, since the provisioner may run as root against a
// workload-writable home.
func WriteHarnessProvisionStatus(path, state, errMsg string) error {
	st := provisionStatus{SchemaVersion: 1, State: state}
	if state == ProvisionStatusFailed {
		st.Error = sanitizeProvisionError(errMsg)
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// The in-memory outputs directory (memOutputsDir) is the one directory
	// this may create, and only for a non-root writer: see
	// writeProvisionStatusInMemOutputs.
	if provisionStatusCreatesDir(path) {
		return writeProvisionStatusInMemOutputs(path, data)
	}
	// Any other outputs directory is never created here: the host stages
	// the bundle's outputs directory, and a directory created by a root-run
	// provisioner in the workload's home could outlive a failed start that
	// skips the post-hook ownership fixup. A root writer never creates the
	// in-memory one either: a root init has already created it for the
	// workload (stagedsecrets.PrepareHarnessDir). A missing directory makes
	// the write fail, which callers treat as best-effort. The status file
	// itself may be left root-owned after such a start; it sits in the
	// workload-owned outputs directory, so it stays removable, and the host
	// clears the bundle on the next start.
	return dirfd.WriteFileNoFollow(path, data, 0o600, 0, 0, dirfd.ReplaceLeaf)
}

// ErrProvisionStatusDirUnsafe reports that the in-memory outputs directory
// exists but is not one the provision status may be written into: not a
// real directory (a symlink or another file type), not owned by the
// writer, or with a mode wider than 0700. The status write is skipped; the
// directory is never repaired.
var ErrProvisionStatusDirUnsafe = errors.New("in-memory harness outputs directory is not safe for the provision status")

// memOutputsName is the name of the in-memory outputs directory below the
// in-memory root, the runtimes' SCION_HARNESS_OUTPUTS_DIR
// (pkg/runtime k8sHarnessOutputsDir).
const memOutputsName = "outputs"

// provisionStatusGeteuid is os.Geteuid's call site as a package var: a seam,
// so a test can drive the root and non-root writer paths and the ownership
// check whatever uid the test process runs as.
var provisionStatusGeteuid = os.Geteuid

// SetProvisionStatusGeteuidForTest replaces the effective uid the provision
// status writer sees and returns a function that restores it. For tests
// only.
func SetProvisionStatusGeteuidForTest(geteuid func() int) (restore func()) {
	prev := provisionStatusGeteuid
	provisionStatusGeteuid = geteuid
	return func() { provisionStatusGeteuid = prev }
}

// memOutputsDir returns the one directory the provision status writer may
// create: memOutputsName directly below the in-memory root.
func memOutputsDir() string {
	return filepath.Join(filepath.Clean(harnessDirsRoot), memOutputsName)
}

// provisionStatusCreatesDir reports whether the status write to path may
// create its parent directory: only when the writer is not root and the
// parent is exactly memOutputsDir. That directory lives on the pod's or
// instance's in-memory volume, never in the workload's home, and is
// discarded with it. An unprivileged init leaves its creation to the
// harness (stagedsecrets.PrepareHarnessDir), and the status write runs
// before the provisioner script, so without this the first status of an
// unprivileged start (Kubernetes NFS-home pods) is never recorded.
func provisionStatusCreatesDir(path string) bool {
	if provisionStatusGeteuid() == 0 || !filepath.IsAbs(path) {
		return false
	}
	return filepath.Dir(filepath.Clean(path)) == memOutputsDir()
}

// HarnessProvisionFailureDetail returns the error recorded by a failed
// `sciontool harness provision` run for the agent home, or "" when there is
// none (no file, not failed, unreadable or malformed).
func HarnessProvisionFailureDetail(agentHome string) string {
	if agentHome == "" {
		return ""
	}
	path, err := HarnessProvisionStatusPath(filepath.Join(agentHome, ".scion", "harness"))
	if err != nil {
		return ""
	}
	data, err := dirfd.ReadFileNoFollow(path, provisionStatusMaxFile)
	if err != nil {
		return ""
	}
	var st provisionStatus
	if err := json.Unmarshal(data, &st); err != nil || st.State != ProvisionStatusFailed {
		return ""
	}
	return sanitizeProvisionError(st.Error)
}

// annotateHarnessProvisionError appends the recorded provisioner error to a
// failed 20-harness-provision pre-start hook's error. err stays wrapped, so
// errors.Is/As on it are unaffected.
func (m *LifecycleManager) annotateHarnessProvisionError(eventName, scriptPath string, err error) error {
	if err == nil || eventName != EventPreStart || filepath.Base(scriptPath) != HarnessProvisionHookFilename {
		return err
	}
	if errors.Is(err, ErrScriptRefused) {
		return err
	}
	home := m.AgentHome
	if home == "" {
		home = os.Getenv("HOME")
	}
	detail := HarnessProvisionFailureDetail(home)
	if detail == "" {
		return err
	}
	return &provisionHookError{err: err, detail: detail}
}

type provisionHookError struct {
	err    error
	detail string
}

func (e *provisionHookError) Error() string { return e.err.Error() + ": " + e.detail }
func (e *provisionHookError) Unwrap() error { return e.err }

// sanitizeProvisionError collapses control characters (including newlines)
// to single spaces and truncates on a rune boundary so the result, including
// the trailing ellipsis that marks a cut, is at most provisionStatusMaxError
// bytes.
func sanitizeProvisionError(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.IsSpace(r) {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) <= provisionStatusMaxError {
		return out
	}
	const ellipsis = "…"
	cut := provisionStatusMaxError - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + ellipsis
}

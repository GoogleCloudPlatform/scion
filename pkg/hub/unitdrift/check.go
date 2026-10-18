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

package unitdrift

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTimeout bounds the whole check, including every command it runs.
const DefaultTimeout = 10 * time.Second

// UnitInfo is what systemd reports about the installed unit.
type UnitInfo struct {
	// FragmentPath is the unit file systemd loaded; empty if none.
	FragmentPath string
	// DropInPaths lists drop-in files that also apply.
	DropInPaths []string
	// NeedDaemonReload is true when the file changed on disk since systemd
	// last loaded it.
	NeedDaemonReload bool
}

// Locator finds the installed unit for a service.
type Locator func(ctx context.Context, service string) (UnitInfo, error)

// SystemctlLocator asks systemd for the unit's file and drop-ins with an
// unprivileged "systemctl show".
func SystemctlLocator(ctx context.Context, service string) (UnitInfo, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show",
		"-p", "FragmentPath", "-p", "DropInPaths", "-p", "NeedDaemonReload",
		service).Output()
	if err != nil {
		return UnitInfo{}, fmt.Errorf("systemctl show: %w", err)
	}
	return parseShow(string(out)), nil
}

func parseShow(out string) UnitInfo {
	var info UnitInfo
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "FragmentPath":
			info.FragmentPath = value
		case "DropInPaths":
			info.DropInPaths = strings.Fields(value)
		case "NeedDaemonReload":
			info.NeedDaemonReload = value == "yes"
		}
	}
	return info
}

// Revision returns the short commit of the checkout at repoPath.
type Revision func(ctx context.Context, repoPath string) (string, error)

// GitRevision runs "git rev-parse --short HEAD" in repoPath.
func GitRevision(ctx context.Context, repoPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CheckOptions configures Check.
type CheckOptions struct {
	RepoPath string // checkout holding the template
	Branch   string // branch the rebuild used (shown in the remediation); may be empty
	Service  string // systemd service name, e.g. "scion-hub"
	Locate   Locator
	Revision Revision
	Timeout  time.Duration // DefaultTimeout when zero
}

// DriftWarning is the first line of the report when the unit differs.
const DriftWarning = "WARNING: installed unit differs from template: re-provision needed"

// Check compares the installed unit with the template in the checkout and
// returns a report for the run log. It never fails and never panics: any
// problem, including a timeout, becomes a "skipped" report, so a caller can
// always go on to restart the service.
func Check(ctx context.Context, opts CheckOptions) (report string) {
	defer func() {
		if r := recover(); r != nil {
			report = skipped(fmt.Sprintf("internal error: %v", r))
		}
	}()

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	template, err := readLimited(filepath.Join(opts.RepoPath, TemplatePath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skipped("no unit template in the checked-out tree (" + TemplatePath + ")")
		}
		return skipped(fmt.Sprintf("cannot read the unit template: %v", err))
	}
	if opts.Locate == nil {
		return skipped("no unit locator configured")
	}
	info, err := locateBounded(ctx, opts.Locate, opts.Service)
	if err != nil {
		return skipped(fmt.Sprintf("cannot locate the installed unit for %s: %v", opts.Service, err))
	}
	if info.FragmentPath == "" {
		return skipped(fmt.Sprintf("%s is not loaded from a unit file", opts.Service))
	}
	installed, err := readLimited(info.FragmentPath)
	if err != nil {
		return skipped(fmt.Sprintf("cannot read the installed unit: %v", err))
	}

	res := Compare(template, installed)
	var b strings.Builder
	switch res.Status {
	case Skipped:
		return skipped(res.Reason)
	case Match:
		b.WriteString("Systemd unit matches template.\n")
	case Drift:
		commit := ""
		if opts.Revision != nil {
			if c, err := opts.Revision(ctx, opts.RepoPath); err == nil && c != "" {
				commit = c
			}
		}
		writeDrift(&b, opts.Service, opts.Branch, commit, res.Diff)
	}
	if len(info.DropInPaths) > 0 {
		fmt.Fprintf(&b, "Note: drop-ins also apply and can override template keys, also after re-provisioning; they were not compared: %s\n",
			strings.Join(info.DropInPaths, " "))
	}
	if info.NeedDaemonReload {
		b.WriteString("Note: the unit file changed on disk but systemd has not reloaded it (systemctl daemon-reload needed).\n")
	}
	return b.String()
}

// locateBounded runs the locator in its own goroutine so that a locator
// that ignores ctx still cannot hold the caller past the deadline, and a
// panic in it is turned into an error rather than crashing the process.
func locateBounded(ctx context.Context, locate Locator, service string) (UnitInfo, error) {
	type result struct {
		info UnitInfo
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("internal error: %v", r)}
			}
		}()
		info, err := locate(ctx, service)
		ch <- result{info, err}
	}()
	select {
	case r := <-ch:
		if r.err == nil && ctx.Err() != nil {
			return UnitInfo{}, fmt.Errorf("timed out: %w", ctx.Err())
		}
		return r.info, r.err
	case <-ctx.Done():
		return UnitInfo{}, fmt.Errorf("timed out: %w", ctx.Err())
	}
}

// maxUnitBytes bounds how much of a unit file is read.
const maxUnitBytes = 1 << 20

// readLimited reads at most maxUnitBytes of path; a larger file is an error.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxUnitBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxUnitBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxUnitBytes)
	}
	return b, nil
}

func skipped(reason string) string {
	return "Systemd unit check skipped: " + reason + ".\n"
}

func writeDrift(b *strings.Builder, service, branch, commit string, diff []string) {
	rev := "the checked-out commit"
	if commit != "" {
		rev = "commit " + commit
	}
	if branch != "" {
		rev += " (branch " + branch + ")"
	}
	fmt.Fprintf(b, "%s\n", DriftWarning)
	fmt.Fprintf(b, "The systemd unit for %s was installed from an older template. Rebuild from git updates the binary only; it does not change the unit.\n", service)
	fmt.Fprintf(b, "To apply the template from %s:\n", rev)
	b.WriteString("  1. On the workstation and checkout used to provision this hub (not on the hub VM), check out that commit.\n")
	b.WriteString("  2. Run scripts/starter-hub/gce-start-hub.sh --full with the same flags and environment used at provisioning (for example --no-tls, HUB_BASE_URL and any hub-config.sh overrides). Running it without the original flags can reconfigure the hub (for example enable TLS on a --no-tls hub).\n")
	b.WriteString("  3. Note that --full also re-applies settings.yaml, hub.env (if present on the workstation), the Caddyfile and the sudoers rules from that workstation, then pulls, builds and restarts the hub.\n")
	fmt.Fprintf(b, "Drop-ins under %s.service.d can still override template keys after re-provisioning.\n", strings.TrimSuffix(service, ".service"))
	b.WriteString("Differences (values of Environment and credential-like keys elided):\n")
	for _, d := range diff {
		fmt.Fprintf(b, "  %s\n", d)
	}
}

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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/util"
)

// buildCommonRunArgs constructs the common arguments for 'run' command across different runtimes.
func buildCommonRunArgs(config RunConfig) ([]string, error) {
	args := []string{"run", "-d", "-i"}
	addArg := func(flag string, values ...string) {
		for _, v := range values {
			args = append(args, flag, v)
		}
	}
	addEnv := func(name, value string) {
		if value != "" {
			addArg("-e", fmt.Sprintf("%s=%s", name, value))
		}
	}

	hostHome, _ := os.UserHomeDir()
	expandPath := func(path string, isTarget bool) string {
		// Expand environment variables first (e.g., ${GOPATH}, $HOME)
		expanded, _ := util.ExpandEnv(path)

		// Then handle tilde expansion
		if strings.HasPrefix(expanded, "~/") {
			if isTarget {
				return filepath.Join(util.GetHomeDir(config.UnixUsername), expanded[2:])
			}
			return filepath.Join(hostHome, expanded[2:])
		}
		if expanded == "~" {
			if isTarget {
				return util.GetHomeDir(config.UnixUsername)
			}
			return hostHome
		}
		return expanded
	}

	// Volume deduplication
	volumeMap := make(map[string]string)
	var volumeOrder []string

	registerMount := func(src, tgt string, ro bool, overwrite bool) {
		val := fmt.Sprintf("%s:%s", src, tgt)
		if ro {
			val += ":ro"
		}
		if _, exists := volumeMap[tgt]; !exists {
			volumeOrder = append(volumeOrder, tgt)
			volumeMap[tgt] = val
		} else if overwrite {
			volumeMap[tgt] = val
		}
	}

	var fuseMounts []string
	type gcsVolInfo struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
	}
	var gcsVolumes []gcsVolInfo

	addVolume := func(v api.VolumeMount) {
		tgt := expandPath(v.Target, true)

		if v.Type == "gcs" {
			// Do not register as docker bind mount
			cmd := fmt.Sprintf("mkdir -p %q && gcsfuse ", tgt)
			if v.Prefix != "" {
				cmd += fmt.Sprintf("--only-dir %q ", v.Prefix)
			}
			if v.Mode != "" {
				cmd += fmt.Sprintf("-o %q ", v.Mode)
			}
			// Add implicit dirs for better compatibility with folder structures created via UI/API
			cmd += "--implicit-dirs "

			cmd += fmt.Sprintf("%q %q", v.Bucket, tgt)
			fuseMounts = append(fuseMounts, cmd)

			gcsVolumes = append(gcsVolumes, gcsVolInfo{
				Source: expandPath(v.Source, false),
				Target: tgt,
				Bucket: v.Bucket,
				Prefix: v.Prefix,
			})
			return
		}

		src := expandPath(v.Source, false)
		// Generic volumes from config should NOT overwrite already registered mounts (like workspace)
		registerMount(src, tgt, v.ReadOnly, false)
	}

	addArg("--name", config.Name)

	if config.HomeDir != "" {
		registerMount(config.HomeDir, util.GetHomeDir(config.UnixUsername), false, true)
	}
	if config.RepoRoot != "" && config.Workspace != "" {
		relWorkspace, err := filepath.Rel(config.RepoRoot, config.Workspace)
		if err == nil && !strings.HasPrefix(relWorkspace, "..") {
			// Mount .git
			registerMount(filepath.Join(config.RepoRoot, ".git"), "/repo-root/.git", false, true)
			// Mount workspace at same relative path
			containerWorkspace := filepath.Join("/repo-root", relWorkspace)
			registerMount(config.Workspace, containerWorkspace, false, true)
			addArg("--workdir", containerWorkspace)
		} else {
			// Fallback if workspace is outside repo root or relative path is not straightforward.
			// Still mount RepoRoot so that .git worktree pointers can potentially be resolved if
			// we are clever, but for now just mount both.
			registerMount(config.RepoRoot, "/repo-root", false, true)
			registerMount(config.Workspace, "/workspace", false, true)
			addArg("--workdir", "/workspace")
		}
	} else if config.Workspace != "" {
		registerMount(config.Workspace, "/workspace", false, true)
		addArg("--workdir", "/workspace")
	}

	// Add generic volumes from config, deduplicating among themselves first
	// but respecting already registered mounts.
	dedupedVolumes := make(map[string]api.VolumeMount)
	var dedupedOrder []string
	for _, v := range config.Volumes {
		tgt := expandPath(v.Target, true)
		if _, exists := dedupedVolumes[tgt]; !exists {
			dedupedOrder = append(dedupedOrder, tgt)
		}
		dedupedVolumes[tgt] = v
	}
	for _, tgt := range dedupedOrder {
		addVolume(dedupedVolumes[tgt])
	}

	// If workdir was not set by the RepoRoot/Workspace logic above, check if we have an explicit
	// volume mount for /workspace and if so set workdir to it.
	workdirSet := false
	for _, arg := range args {
		if arg == "--workdir" {
			workdirSet = true
			break
		}
	}
	if !workdirSet {
		for _, v := range dedupedVolumes {
			if expandPath(v.Target, true) == "/workspace" {
				addArg("--workdir", "/workspace")
				break
			}
		}
	}

	// Use Harness for file propagation and env
	if config.Harness != nil {
		if config.HomeDir != "" {
			if err := config.Harness.PropagateFiles(config.HomeDir, config.UnixUsername, config.Auth); err != nil {
				return nil, err
			}
		} else {
			for _, v := range config.Harness.GetVolumes(config.UnixUsername, config.Auth) {
				addVolume(v)
			}
		}
		for k, v := range config.Harness.GetEnv(config.Name, config.HomeDir, config.UnixUsername, config.Auth) {
			addEnv(k, v)
		}
	}

	// Pass host user UID/GID for container user synchronization
	addEnv("SCION_HOST_UID", fmt.Sprintf("%d", os.Getuid()))
	addEnv("SCION_HOST_GID", fmt.Sprintf("%d", os.Getgid()))

	// Mount gcloud config if it exists
	home, _ := os.UserHomeDir()
	gcloudConfigDir := filepath.Join(home, ".config", "gcloud")
	if _, err := os.Stat(gcloudConfigDir); err == nil {
		registerMount(gcloudConfigDir, fmt.Sprintf("/home/%s/.config/gcloud", config.UnixUsername), true, false)
	}

	for _, e := range config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			addArg("-e", fmt.Sprintf("%s=%s", parts[0], parts[1]))
		} else {
			addArg("-e", e)
		}
	}

	// Add all registered volumes
	for _, tgt := range volumeOrder {
		addArg("-v", volumeMap[tgt])
	}

	if len(fuseMounts) > 0 {
		addArg("--cap-add", "SYS_ADMIN")
		addArg("--device", "/dev/fuse")
		if data, err := json.Marshal(gcsVolumes); err == nil {
			encoded := base64.StdEncoding.EncodeToString(data)
			addArg("--label", fmt.Sprintf("scion.gcs_volumes=%s", encoded))
		}
	}

	if config.UseTmux {
		if config.Labels == nil {
			config.Labels = make(map[string]string)
		}
		config.Labels["scion.tmux"] = "true"
	}

	for k, v := range config.Labels {
		addArg("--label", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range config.Annotations {
		addArg("--label", fmt.Sprintf("%s=%s", k, v))
	}
	if config.Template != "" {
		addArg("--label", fmt.Sprintf("scion.template=%s", config.Template))
	}

	args = append(args, config.Image)

	// Get command from harness
	var harnessArgs []string
	if config.Harness != nil {
		harnessArgs = config.Harness.GetCommand(config.Task, config.Resume, config.CommandArgs)
	} else {
		return nil, fmt.Errorf("no harness provided")
	}

	if len(fuseMounts) > 0 {
		var finalCmd []string
		if config.UseTmux {
			var quotedArgs []string
			for _, a := range harnessArgs {
				if strings.ContainsAny(a, " \t\n\"'$") {
					quotedArgs = append(quotedArgs, fmt.Sprintf("%q", a))
				} else {
					quotedArgs = append(quotedArgs, a)
				}
			}
			cmdLine := strings.Join(quotedArgs, " ")
			finalCmd = []string{"tmux", "new-session", "-s", "scion", cmdLine}
		} else {
			finalCmd = harnessArgs
		}

		mountCmds := strings.Join(fuseMounts, " && ")
		var quotedFinal []string
		for _, a := range finalCmd {
			quotedFinal = append(quotedFinal, fmt.Sprintf("%q", a))
		}
		wrapped := fmt.Sprintf("%s && exec %s", mountCmds, strings.Join(quotedFinal, " "))
		args = append(args, "sh", "-c", wrapped)

	} else {
		if config.UseTmux {
			var quotedArgs []string
			for _, a := range harnessArgs {
				// Use %q to quote arguments that might have spaces or special characters
				if strings.ContainsAny(a, " \t\n\"'$") {
					quotedArgs = append(quotedArgs, fmt.Sprintf("%q", a))
				} else {
					quotedArgs = append(quotedArgs, a)
				}
			}
			cmdLine := strings.Join(quotedArgs, " ")
			args = append(args, "tmux", "new-session", "-s", "scion", cmdLine)
		} else {
			args = append(args, harnessArgs...)
		}
	}

	return args, nil
}

func runSimpleCommand(ctx context.Context, command string, args ...string) (string, error) {
	util.Debugf("%s %s", command, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s failed: %w", command, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func runInteractiveCommand(command string, args ...string) error {
	util.Debugf("%s %s", command, strings.Join(args, " "))
	cmd := exec.Command(command, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
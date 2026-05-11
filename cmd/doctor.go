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

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check system prerequisites and runtime configuration",
	Long: `Run diagnostic checks to verify that your system is properly configured
for running scion agents. Checks include runtime availability, connectivity,
permissions, and required dependencies.

For Kubernetes runtimes, this includes cluster connectivity, namespace access,
RBAC permissions, and CSI driver availability.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDoctor()
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor() error {
	fmt.Printf("%sScion Doctor%s\n\n", util.Bold, util.Reset)

	// General checks
	fmt.Printf("%sGeneral%s\n", util.Bold, util.Reset)
	checkGit()
	checkTmux()

	// Resolve the active runtime
	fmt.Printf("\n%sRuntime%s\n", util.Bold, util.Reset)

	resolved, err := resolveActiveProjectPath()
	if err != nil {
		printCheck("project", "warn", "No project found — skipping runtime checks", "Run 'scion init' to create a project.")
		if outputFormat == "json" {
			return outputDoctorJSON(nil)
		}
		return nil
	}

	rt := scionruntime.GetRuntime(resolved, profile)
	rtName := rt.Name()
	printCheck("runtime", "pass", fmt.Sprintf("Active runtime: %s", rtName), "")

	// Runtime-specific diagnostics
	if diag, ok := rt.(scionruntime.Diagnosable); ok {
		// Load settings for runtime config
		var namespace string
		var gkeMode bool
		projectDir, _ := config.GetResolvedProjectDir(resolved)
		if vs, _, err := config.LoadEffectiveSettings(projectDir); err == nil && vs != nil {
			rtConfig, _, rtErr := vs.ResolveRuntime(profile)
			if rtErr == nil {
				namespace = rtConfig.Namespace
				gkeMode = rtConfig.GKE
			}
		}

		opts := scionruntime.DiagnosticOpts{
			Namespace: namespace,
			GKEMode:   gkeMode,
		}

		fmt.Printf("\n%sRuntime Diagnostics (%s)%s\n", util.Bold, rtName, util.Reset)
		report := diag.RunDiagnostics(opts)

		if outputFormat == "json" {
			return outputDoctorJSON(&report)
		}

		for _, check := range report.Checks {
			printCheck(check.Name, check.Status, check.Message, check.Remediation)
		}

		// Summary
		fmt.Println()
		passes, warns, fails := 0, 0, 0
		for _, c := range report.Checks {
			switch c.Status {
			case "pass":
				passes++
			case "warn":
				warns++
			case "fail":
				fails++
			}
		}

		if fails > 0 {
			fmt.Printf("%s%d checks passed, %d warnings, %d failures%s\n",
				util.Red, passes, warns, fails, util.Reset)
			return fmt.Errorf("%d diagnostic check(s) failed", fails)
		}
		if warns > 0 {
			fmt.Printf("%s%d checks passed, %d warnings%s\n",
				util.Yellow, passes, warns, util.Reset)
		} else {
			fmt.Printf("%s%d checks passed%s\n",
				util.Green, passes, util.Reset)
		}
	} else {
		// Non-diagnosable runtimes get basic checks
		switch rtName {
		case "docker":
			checkDockerOrPodman("docker")
		case "podman":
			checkDockerOrPodman("podman")
		case "container":
			checkContainerCLI()
		}
	}

	return nil
}

func resolveActiveProjectPath() (string, error) {
	if projectPath != "" {
		return projectPath, nil
	}
	resolved, _, err := config.RequireProjectPath("")
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func checkGit() {
	path, err := exec.LookPath("git")
	if err != nil {
		printCheck("git", "fail", "git not found in PATH", "Install git: https://git-scm.com/downloads")
		return
	}
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		printCheck("git", "warn", fmt.Sprintf("git found at %s but version check failed", path), "")
		return
	}
	printCheck("git", "pass", trimNewline(string(out)), "")
}

func checkTmux() {
	_, err := exec.LookPath("tmux")
	if err != nil {
		if goruntime.GOOS == "darwin" || goruntime.GOOS == "linux" {
			printCheck("tmux", "warn", "tmux not found locally (required inside agent containers)", "")
		} else {
			printCheck("tmux", "skip", "tmux check skipped on this platform", "")
		}
		return
	}
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		printCheck("tmux", "pass", "tmux found", "")
		return
	}
	printCheck("tmux", "pass", trimNewline(string(out)), "")
}

func checkDockerOrPodman(name string) {
	_, err := exec.LookPath(name)
	if err != nil {
		printCheck(name, "fail", fmt.Sprintf("%s not found in PATH", name), fmt.Sprintf("Install %s", name))
		return
	}
	out, err := exec.Command(name, "--version").Output()
	if err != nil {
		printCheck(name, "warn", fmt.Sprintf("%s found but version check failed", name), "")
		return
	}
	printCheck(name, "pass", trimNewline(string(out)), "")

	// Check daemon connectivity
	_, err = exec.Command(name, "info").Output()
	if err != nil {
		printCheck(name+"-daemon", "fail",
			fmt.Sprintf("%s daemon is not running or not accessible", name),
			fmt.Sprintf("Start the %s daemon and try again.", name))
		return
	}
	printCheck(name+"-daemon", "pass", fmt.Sprintf("%s daemon is running", name), "")
}

func checkContainerCLI() {
	if goruntime.GOOS != "darwin" {
		printCheck("container", "skip", "Apple container CLI is only available on macOS", "")
		return
	}
	_, err := exec.LookPath("container")
	if err != nil {
		printCheck("container", "fail", "container CLI not found in PATH", "Install the container CLI for macOS")
		return
	}
	printCheck("container", "pass", "container CLI found", "")
}

func printCheck(name, status, message, remediation string) {
	var icon string
	switch status {
	case "pass":
		icon = fmt.Sprintf("%s✓%s", util.Green, util.Reset)
	case "warn":
		icon = fmt.Sprintf("%s!%s", util.Yellow, util.Reset)
	case "fail":
		icon = fmt.Sprintf("%s✗%s", util.Red, util.Reset)
	case "skip":
		icon = fmt.Sprintf("%s-%s", util.Gray, util.Reset)
	}
	fmt.Printf("  %s %s: %s\n", icon, name, message)
	if remediation != "" && status != "pass" {
		fmt.Printf("    → %s\n", remediation)
	}
}

func outputDoctorJSON(report *scionruntime.DiagnosticReport) error {
	out := map[string]interface{}{}
	if report != nil {
		out["runtime"] = report.Runtime
		out["checks"] = report.Checks
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, string(data))
	return nil
}

func trimNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}

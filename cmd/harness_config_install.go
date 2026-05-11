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
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var harnessConfigInstallCmd = &cobra.Command{
	Use:   "install <source>",
	Short: "Install a harness-config from a URL or local path",
	Long: `Downloads and installs a harness-config from a remote or local source.

Source can be:
  - A GitHub URL pointing to a directory containing config.yaml
  - A shorthand GitHub URL (github.com/org/repo/tree/main/path)
  - A file:// URL for a local directory
  - An rclone URI (:gcs:bucket/path)
  - An archive URL (.tgz, .zip)

The installed name defaults to the source directory name, override with --name.

Examples:
  scion harness-config install https://github.com/org/repo/tree/main/harness-configs/my-config
  scion harness-config install github.com/org/repo/tree/main/harness-configs/custom-claude
  scion harness-config install file:///path/to/my-config
  scion harness-config install :gcs:my-bucket/harness-configs/prod-claude
  scion harness-config install --name my-claude --global https://github.com/org/repo/tree/main/custom`,
	Args: cobra.ExactArgs(1),
	RunE: runHarnessConfigInstall,
}

func runHarnessConfigInstall(cmd *cobra.Command, args []string) error {
	source := args[0]
	nameOverride, _ := cmd.Flags().GetString("name")
	force, _ := cmd.Flags().GetBool("force")

	var gp string
	if projectPath != "" {
		resolved, err := config.GetResolvedProjectDir(projectPath)
		if err == nil {
			gp = resolved
		}
	} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
		gp = projectDir
	}

	localSourcePath, cleanup, err := resolveInstallSource(source)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	hcDir, err := config.LoadHarnessConfigDir(localSourcePath)
	if err != nil {
		return fmt.Errorf("invalid harness-config source: %w", err)
	}

	name := nameOverride
	if name == "" {
		name = deriveHarnessConfigName(source)
	}
	if name == "" || name == "." || name == "/" {
		return fmt.Errorf("could not derive harness-config name from source %q; use --name to specify", source)
	}

	hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
	if err != nil {
		return err
	}

	if hubCtx != nil {
		return installToHub(hubCtx, name, localSourcePath, hcDir.Config.Harness)
	}

	return installLocally(name, localSourcePath, gp, force, hcDir.Config.Harness)
}

func resolveInstallSource(source string) (localPath string, cleanup func(), err error) {
	if strings.HasPrefix(source, "file://") {
		localPath = strings.TrimPrefix(source, "file://")
		localPath, err = filepath.Abs(localPath)
		if err != nil {
			return "", nil, fmt.Errorf("failed to resolve file path: %w", err)
		}
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			return "", nil, fmt.Errorf("source not found: %s", localPath)
		}
		if !info.IsDir() {
			return "", nil, fmt.Errorf("source is not a directory: %s", localPath)
		}
		return localPath, nil, nil
	}

	normalized := normalizeHarnessConfigSourceURL(source)
	if config.IsRemoteURI(normalized) {
		if err := config.ValidateRemoteURI(normalized); err != nil {
			return "", nil, fmt.Errorf("invalid remote source: %w", err)
		}

		fmt.Printf("Fetching remote source: %s\n", source)
		cachePath, fetchErr := config.FetchRemoteTemplate(context.Background(), normalized)
		if fetchErr != nil {
			return "", nil, fmt.Errorf("failed to fetch remote source: %w", fetchErr)
		}
		return cachePath, func() { os.RemoveAll(cachePath) }, nil
	}

	localPath, err = filepath.Abs(source)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve source path: %w", err)
	}
	info, statErr := os.Stat(localPath)
	if statErr != nil {
		return "", nil, fmt.Errorf("source not found: %s", source)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("source is not a directory: %s", source)
	}
	return localPath, nil, nil
}

// normalizeHarnessConfigSourceURL normalizes a user-provided source URL.
// It prepends https:// for bare hostnames (e.g. "github.com/org/repo/...").
func normalizeHarnessConfigSourceURL(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "file://") {
		return s
	}
	if !strings.HasPrefix(s, ":") && !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		if strings.Contains(s, "github.com") || strings.Contains(s, "/") && strings.Contains(strings.SplitN(s, "/", 2)[0], ".") {
			s = "https://" + s
		}
	}
	return s
}

func installToHub(hubCtx *HubContext, name, localPath, harnessType string) error {
	PrintUsingHub(hubCtx.Endpoint)

	scope := "grove"
	scopeID := ""

	if globalMode {
		scope = "global"
	} else {
		projectID, err := GetProjectID(hubCtx)
		if err != nil {
			return fmt.Errorf("failed to resolve project for Hub install: %w", err)
		}
		scopeID = projectID
	}

	return syncHarnessConfigToHub(hubCtx, name, localPath, scope, scopeID, harnessType)
}

func installLocally(name, sourcePath, projectPath string, force bool, harnessType string) error {
	var destDir string
	if globalMode {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return fmt.Errorf("failed to resolve global directory: %w", err)
		}
		destDir = filepath.Join(globalDir, "harness-configs", name)
	} else if projectPath != "" {
		destDir = filepath.Join(projectPath, "harness-configs", name)
	} else {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return fmt.Errorf("failed to resolve global directory: %w", err)
		}
		destDir = filepath.Join(globalDir, "harness-configs", name)
		fmt.Fprintf(os.Stderr, "No grove found, installing to global: %s\n", destDir)
	}

	if info, err := os.Stat(destDir); err == nil && info.IsDir() {
		if !force {
			return fmt.Errorf("harness-config %q already exists at %s; use --force to overwrite", name, destDir)
		}
		if err := os.RemoveAll(destDir); err != nil {
			return fmt.Errorf("failed to remove existing harness-config: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	if err := util.CopyDir(sourcePath, destDir); err != nil {
		return fmt.Errorf("failed to install harness-config: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "harness-config install",
			Message: fmt.Sprintf("Harness-config '%s' installed successfully.", name),
			Details: map[string]interface{}{
				"name":        name,
				"harness":     harnessType,
				"destination": destDir,
			},
		})
	}

	fmt.Printf("Harness-config '%s' installed to %s\n", name, destDir)
	return nil
}

// deriveHarnessConfigName extracts a harness-config name from a source URL or path.
func deriveHarnessConfigName(source string) string {
	if strings.HasPrefix(source, "file://") {
		localPath := strings.TrimPrefix(source, "file://")
		return filepath.Base(filepath.Clean(localPath))
	}

	if strings.HasPrefix(source, ":") {
		parts := strings.SplitN(source, ":", 3)
		if len(parts) == 3 {
			return path.Base(strings.TrimRight(parts[2], "/"))
		}
		return ""
	}

	normalized := normalizeHarnessConfigSourceURL(source)
	u, err := url.Parse(normalized)
	if err != nil {
		return filepath.Base(filepath.Clean(source))
	}

	cleanPath := strings.TrimRight(u.Path, "/")
	if cleanPath == "" {
		return filepath.Base(filepath.Clean(source))
	}
	return path.Base(cleanPath)
}

func init() {
	harnessConfigInstallCmd.Flags().String("name", "", "Override the installed harness-config name")
	harnessConfigInstallCmd.Flags().Bool("force", false, "Overwrite an existing local harness-config")
}

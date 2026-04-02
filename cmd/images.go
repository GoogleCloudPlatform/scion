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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	imagesBuildRegistry string
	imagesBuildTarget   string
	imagesBuildPush     bool
	imagesBuildPlatform string
	imagesBuildTag      string
	imagesBuildSource   string
)

var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Manage Scion container images",
	Long:  `Commands for building and managing Scion container images.`,
}

var imagesBuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build Scion container images using docker buildx",
	Long: `Build Scion container images locally using docker buildx.

Builds the Scion container image hierarchy:

  core-base          System dependencies (Go, Node, Python)
    └── scion-base   Adds sciontool binary and scion user
          ├── claude     Claude Code harness
          ├── gemini     Gemini CLI harness
          ├── opencode   OpenCode harness
          └── codex      Codex harness

The --source flag (or current directory) must point to a clone of the
Scion source repository, as the Dockerfiles are located there.

After building, configure scion to use the registry:
  scion config set image_registry <registry>`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runImagesBuild()
	},
}

func init() {
	rootCmd.AddCommand(imagesCmd)
	imagesCmd.AddCommand(imagesBuildCmd)

	imagesBuildCmd.Flags().StringVar(&imagesBuildRegistry, "registry", "", "Target registry path, e.g. ghcr.io/myorg (required)")
	_ = imagesBuildCmd.MarkFlagRequired("registry")
	imagesBuildCmd.Flags().StringVar(&imagesBuildTarget, "target", "common", "Build target: common (scion-base + harnesses), all (full rebuild including core-base), core-base, harnesses")
	imagesBuildCmd.Flags().BoolVar(&imagesBuildPush, "push", false, "Push images to the registry after building")
	imagesBuildCmd.Flags().StringVar(&imagesBuildPlatform, "platform", "", `Target platform(s): "all" (linux/amd64,linux/arm64) or explicit e.g. "linux/amd64"`)
	imagesBuildCmd.Flags().StringVar(&imagesBuildTag, "tag", "latest", "Image tag")
	imagesBuildCmd.Flags().StringVar(&imagesBuildSource, "source", "", "Path to a Scion source repository clone (default: current directory)")
}

func runImagesBuild() error {
	sourceDir := imagesBuildSource
	if sourceDir == "" {
		var err error
		sourceDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("getting current directory: %w", err)
		}
	}

	scriptPath := filepath.Join(sourceDir, "image-build", "scripts", "build-images.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return fmt.Errorf(
			"build script not found at %s\n\n"+
				"'scion images build' requires the Scion source repository because\n"+
				"the Dockerfiles and build scripts are part of the repo.\n\n"+
				"Run this command from within a clone of the Scion repo, or use\n"+
				"--source <path> to specify the repository path.\n\n"+
				"To clone: git clone https://github.com/GoogleCloudPlatform/scion",
			scriptPath,
		)
	}

	scriptArgs := []string{
		scriptPath,
		"--registry", imagesBuildRegistry,
		"--target", imagesBuildTarget,
		"--tag", imagesBuildTag,
	}
	if imagesBuildPush {
		scriptArgs = append(scriptArgs, "--push")
	}
	if imagesBuildPlatform != "" {
		scriptArgs = append(scriptArgs, "--platform", imagesBuildPlatform)
	}

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash not found in PATH: %w", err)
	}

	buildCmd := exec.Command(bashPath, scriptArgs...)
	buildCmd.Dir = sourceDir
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr

	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("image build failed: %w", err)
	}

	return nil
}

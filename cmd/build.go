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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	buildTag       string
	buildBaseImage string
	buildPush      bool
	buildPlatform  string
	buildDryRun    bool
)

var buildCmd = &cobra.Command{
	Use:   "build <harness-config-name>",
	Short: "Build a container image from a harness-config Dockerfile",
	Long: `Build a container image from a Dockerfile bundled inside a harness-config directory.

The base image is resolved from the image_registry setting unless --base-image
is provided. After a successful build the harness-config's config.yaml image
field is updated to reference the built image.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		harnessConfigName := args[0]

		hcDir, err := config.FindHarnessConfigDir(harnessConfigName, projectPath)
		if err != nil {
			return fmt.Errorf("harness-config %q not found: %w", harnessConfigName, err)
		}
		if hcDir.Path == "" {
			return fmt.Errorf("harness-config %q does not have a local directory path", harnessConfigName)
		}

		dockerfilePath := filepath.Join(hcDir.Path, "Dockerfile")
		if _, err := os.Stat(dockerfilePath); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("harness-config %q does not contain a Dockerfile", harnessConfigName)
			}
			return fmt.Errorf("cannot access Dockerfile in harness-config %q: %w", harnessConfigName, err)
		}

		tag := buildTag

		var settings *config.VersionedSettings
		if buildBaseImage == "" || buildPush {
			settings, _, err = config.LoadEffectiveSettings(projectPath)
			if err != nil {
				return fmt.Errorf("failed to load settings: %w", err)
			}
		}

		baseImage := buildBaseImage
		if baseImage == "" {
			imageRegistry := ""
			if settings != nil {
				imageRegistry = settings.ResolveImageRegistry(profile)
			}
			baseImage = "scion-base:" + tag
			if imageRegistry != "" {
				baseImage = imageRegistry + "/scion-base:" + tag
			}
		}

		runtimeBin := runtime.DetectContainerRuntime()
		if runtimeBin == "" {
			return fmt.Errorf("no container runtime found (tried docker, podman)")
		}

		outputImage := harnessConfigName + ":" + tag
		if buildPush {
			imageRegistry := ""
			if settings != nil {
				imageRegistry = settings.ResolveImageRegistry(profile)
			}
			if imageRegistry == "" {
				return fmt.Errorf("--push requires image_registry to be configured")
			}
			outputImage = imageRegistry + "/" + harnessConfigName + ":" + tag
		}

		buildArgs := []string{"build",
			"--build-arg", "BASE_IMAGE=" + baseImage,
			"-t", outputImage,
		}
		if buildPlatform != "" {
			buildArgs = append(buildArgs, "--platform", buildPlatform)
		}
		buildArgs = append(buildArgs, hcDir.Path)

		if buildDryRun {
			fmt.Println(runtimeBin + " " + strings.Join(buildArgs, " "))
			return nil
		}

		buildExec := exec.CommandContext(cmd.Context(), runtimeBin, buildArgs...)
		buildExec.Stdout = os.Stdout
		buildExec.Stderr = os.Stderr
		if err := buildExec.Run(); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}

		if buildPush {
			pushExec := exec.CommandContext(cmd.Context(), runtimeBin, "push", outputImage)
			pushExec.Stdout = os.Stdout
			pushExec.Stderr = os.Stderr
			if err := pushExec.Run(); err != nil {
				return fmt.Errorf("push failed: %w", err)
			}
		}

		configPath := filepath.Join(hcDir.Path, "config.yaml")
		configData, err := os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("failed to read config.yaml for update: %w", err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(configData, &doc); err != nil {
			return fmt.Errorf("failed to parse config.yaml: %w", err)
		}
		if len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
			mapping := doc.Content[0]
			found := false
			for i := 0; i < len(mapping.Content)-1; i += 2 {
				if mapping.Content[i].Value == "image" {
					mapping.Content[i+1].Value = outputImage
					found = true
					break
				}
			}
			if !found {
				mapping.Content = append(mapping.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "image"},
					&yaml.Node{Kind: yaml.ScalarNode, Value: outputImage},
				)
			}
		}
		updatedData, err := yaml.Marshal(&doc)
		if err != nil {
			return fmt.Errorf("failed to marshal updated config.yaml: %w", err)
		}
		if err := os.WriteFile(configPath, updatedData, 0644); err != nil {
			return fmt.Errorf("failed to write updated config.yaml: %w", err)
		}
		fmt.Printf("Updated %s image to %s\n", configPath, outputImage)

		// Sync updated config to Hub so agents pick up the new image.
		var gp string
		if projectPath != "" {
			if resolved, err := config.GetResolvedProjectDir(projectPath); err == nil {
				gp = resolved
			}
		} else if resolved, err := config.GetResolvedProjectDir(""); err == nil {
			gp = resolved
		}
		hubCtx, hubErr := CheckHubAvailabilityWithOptions(gp, true)
		if hubErr != nil {
			fmt.Printf("Warning: could not sync to Hub: %v\n", hubErr)
			fmt.Println("Run 'scion harness-config push " + harnessConfigName + "' to sync manually.")
		} else if hubCtx != nil {
			if err := syncHarnessConfigToHub(hubCtx, harnessConfigName, hcDir.Path, "global", "", hcDir.Config.Harness); err != nil {
				fmt.Printf("Warning: failed to sync to Hub: %v\n", err)
				fmt.Println("Run 'scion harness-config push " + harnessConfigName + "' to sync manually.")
			}
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(buildCmd)
	buildCmd.Flags().StringVar(&buildTag, "tag", "latest", "Image tag")
	buildCmd.Flags().StringVar(&buildBaseImage, "base-image", "", "Override the base image (skips image_registry resolution)")
	buildCmd.Flags().BoolVar(&buildPush, "push", false, "Push built image to image_registry after building")
	buildCmd.Flags().StringVar(&buildPlatform, "platform", "", "Target platform (default: current architecture)")
	buildCmd.Flags().BoolVar(&buildDryRun, "dry-run", false, "Show the docker build command without executing")
}

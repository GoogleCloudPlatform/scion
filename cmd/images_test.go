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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImagesBuildCommandRegistered(t *testing.T) {
	// Verify "scion images" is registered on root
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Name() == "images" {
			found = true
			break
		}
	}
	assert.True(t, found, "images command should be registered on rootCmd")

	// Verify "scion images build" is registered under images
	buildFound := false
	for _, c := range imagesCmd.Commands() {
		if c.Name() == "build" {
			buildFound = true
			break
		}
	}
	assert.True(t, buildFound, "build subcommand should be registered under imagesCmd")
}

func TestImagesBuildFlagsRegistered(t *testing.T) {
	assert.NotNil(t, imagesBuildCmd.Flags().Lookup("registry"))
	assert.NotNil(t, imagesBuildCmd.Flags().Lookup("target"))
	assert.NotNil(t, imagesBuildCmd.Flags().Lookup("push"))
	assert.NotNil(t, imagesBuildCmd.Flags().Lookup("platform"))
	assert.NotNil(t, imagesBuildCmd.Flags().Lookup("tag"))
	assert.NotNil(t, imagesBuildCmd.Flags().Lookup("source"))

	// --registry is required
	ann := imagesBuildCmd.Flags().Lookup("registry").Annotations
	_, required := ann[cobra_requiredAnnotation]
	assert.True(t, required, "--registry should be marked as required")
}

// cobra_requiredAnnotation is the annotation key cobra uses to mark required flags.
const cobra_requiredAnnotation = "cobra_annotation_bash_completion_one_required_flag"

func TestImagesBuildMissingScript(t *testing.T) {
	// Point source to a temp dir that has no image-build subdirectory
	tmpDir := t.TempDir()

	orig := imagesBuildSource
	defer func() { imagesBuildSource = orig }()
	imagesBuildSource = tmpDir

	origRegistry := imagesBuildRegistry
	defer func() { imagesBuildRegistry = origRegistry }()
	imagesBuildRegistry = "ghcr.io/test"

	err := runImagesBuild()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build script not found")
	assert.Contains(t, err.Error(), "image-build/scripts/build-images.sh")
	assert.Contains(t, err.Error(), "--source")
}

func TestImagesBuildInvokesScript(t *testing.T) {
	// Create a fake source tree with a stub build-images.sh that records its args
	tmpDir := t.TempDir()
	scriptDir := filepath.Join(tmpDir, "image-build", "scripts")
	require.NoError(t, os.MkdirAll(scriptDir, 0755))

	// Write a stub script that writes its arguments to a file
	argsFile := filepath.Join(tmpDir, "captured-args")
	stubScript := "#!/bin/bash\necho \"$@\" > " + argsFile + "\n"
	scriptPath := filepath.Join(scriptDir, "build-images.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(stubScript), 0755))

	// Save and restore flag state
	orig := struct {
		source, registry, target, tag, platform string
		push                                    bool
	}{imagesBuildSource, imagesBuildRegistry, imagesBuildTarget, imagesBuildTag, imagesBuildPlatform, imagesBuildPush}
	defer func() {
		imagesBuildSource = orig.source
		imagesBuildRegistry = orig.registry
		imagesBuildTarget = orig.target
		imagesBuildTag = orig.tag
		imagesBuildPlatform = orig.platform
		imagesBuildPush = orig.push
	}()

	imagesBuildSource = tmpDir
	imagesBuildRegistry = "ghcr.io/myorg"
	imagesBuildTarget = "common"
	imagesBuildTag = "latest"
	imagesBuildPlatform = ""
	imagesBuildPush = false

	err := runImagesBuild()
	require.NoError(t, err)

	// Verify the script was called with correct args
	captured, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr, "stub script should have written args file")

	args := strings.TrimSpace(string(captured))
	assert.Contains(t, args, "--registry ghcr.io/myorg")
	assert.Contains(t, args, "--target common")
	assert.Contains(t, args, "--tag latest")
	assert.NotContains(t, args, "--push")
	assert.NotContains(t, args, "--platform")
}

func TestImagesBuildWithPushAndPlatform(t *testing.T) {
	tmpDir := t.TempDir()
	scriptDir := filepath.Join(tmpDir, "image-build", "scripts")
	require.NoError(t, os.MkdirAll(scriptDir, 0755))

	argsFile := filepath.Join(tmpDir, "captured-args")
	stubScript := "#!/bin/bash\necho \"$@\" > " + argsFile + "\n"
	scriptPath := filepath.Join(scriptDir, "build-images.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(stubScript), 0755))

	orig := struct {
		source, registry, target, tag, platform string
		push                                    bool
	}{imagesBuildSource, imagesBuildRegistry, imagesBuildTarget, imagesBuildTag, imagesBuildPlatform, imagesBuildPush}
	defer func() {
		imagesBuildSource = orig.source
		imagesBuildRegistry = orig.registry
		imagesBuildTarget = orig.target
		imagesBuildTag = orig.tag
		imagesBuildPlatform = orig.platform
		imagesBuildPush = orig.push
	}()

	imagesBuildSource = tmpDir
	imagesBuildRegistry = "us-docker.pkg.dev/myproject/scion"
	imagesBuildTarget = "all"
	imagesBuildTag = "v1.0"
	imagesBuildPlatform = "all"
	imagesBuildPush = true

	err := runImagesBuild()
	require.NoError(t, err)

	captured, readErr := os.ReadFile(argsFile)
	require.NoError(t, readErr)

	args := strings.TrimSpace(string(captured))
	assert.Contains(t, args, "--registry us-docker.pkg.dev/myproject/scion")
	assert.Contains(t, args, "--target all")
	assert.Contains(t, args, "--tag v1.0")
	assert.Contains(t, args, "--push")
	assert.Contains(t, args, "--platform all")
}

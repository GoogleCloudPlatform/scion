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

//go:build !no_sqlite

package cmd

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArtifactDeleteRoundTrip: the owner deletes its artifact after
// confirming; afterwards the hub answers 404 for it. A declined prompt
// deletes nothing; without a terminal and without --yes the command fails
// before asking; a version reference is refused.
func TestArtifactDeleteRoundTrip(t *testing.T) {
	svc, _ := shareHub(t, oneUserHost{})
	ctx := context.Background()
	pub, err := svc.Publish(ctx, &hubclient.PublishArtifactRequest{Name: "d.md", Scope: "project-1",
		Content: strings.NewReader("# d"), Size: 3})
	require.NoError(t, err)
	ref := pub.Artifact.Ref

	var out bytes.Buffer
	var asked string
	declined := deleteConfirm{Interactive: true, Ask: func(p string) bool { asked = p; return false }}
	require.NoError(t, deleteArtifact(ctx, svc, &out, ref, declined))
	assert.Contains(t, asked, ref)
	assert.Contains(t, out.String(), "Not deleted")
	_, err = svc.Get(ctx, pub.Artifact.ID)
	require.NoError(t, err, "a declined prompt deletes nothing")

	// No terminal and no --yes: refused with an error, nothing asked or deleted.
	out.Reset()
	noTTY := deleteConfirm{Ask: func(string) bool { t.Error("asked without a terminal"); return true }}
	err = deleteArtifact(ctx, svc, &out, ref, noTTY)
	require.ErrorIs(t, err, errDeleteNeedsConfirmation)
	assert.Contains(t, err.Error(), "--yes")
	assert.Empty(t, out.String())
	_, err = svc.Get(ctx, pub.Artifact.ID)
	require.NoError(t, err, "a refused delete deletes nothing")

	yes := deleteConfirm{Yes: true}
	require.Error(t, deleteArtifact(ctx, svc, &out, ref+"@1", yes))
	require.Error(t, deleteArtifact(ctx, svc, &out, "not-a-ref", yes))

	// --yes needs no terminal and asks nothing.
	out.Reset()
	require.NoError(t, deleteArtifact(ctx, svc, &out, ref, yes))
	assert.Contains(t, out.String(), "Deleted "+ref)
	_, err = svc.Get(ctx, pub.Artifact.ID)
	require.Error(t, err, "the deleted artifact is gone")
	require.Error(t, deleteArtifact(ctx, svc, &out, ref, yes), "a second delete finds nothing")
}

// TestArtifactDeleteRefusedForAgents: the hub refuses deletion to agents,
// whatever the CLI allows.
func TestArtifactDeleteRefusedForAgents(t *testing.T) {
	svc, _ := shareHub(t, oneAgentHost{})
	ctx := context.Background()
	pub, err := svc.Publish(ctx, &hubclient.PublishArtifactRequest{Name: "a.md", Content: strings.NewReader("a"), Size: 1})
	require.NoError(t, err)
	var out bytes.Buffer
	require.Error(t, deleteArtifact(ctx, svc, &out, pub.Artifact.Ref, deleteConfirm{Yes: true}))
	_, err = svc.Get(ctx, pub.Artifact.ID)
	require.NoError(t, err)
}

// TestArtifactDeleteModes: delete is a human-mode command. It is not on
// the agent allow-list, so agent mode removes it (and its line in the
// artifact help) while the agent verbs stay.
func TestArtifactDeleteModes(t *testing.T) {
	assert.False(t, agentAllowed["artifact.delete"])
	build := func() (*cobra.Command, *cobra.Command) {
		root := &cobra.Command{Use: "scion"}
		art := &cobra.Command{Use: "artifact", Long: artifactCmd.Long}
		for _, v := range []string{"delete", "share", "get", "publish", "versions"} {
			art.AddCommand(&cobra.Command{Use: v})
		}
		root.AddCommand(art)
		return root, art
	}
	for mode, want := range map[CLIMode]bool{ModeHuman: true, ModeAgent: false} {
		t.Run(string(mode), func(t *testing.T) {
			root, art := build()
			applyModeRestrictions(root, mode)
			names := collectCommandNames(root)
			assert.Equal(t, want, slices.Contains(names, "artifact.delete"))
			assert.Equal(t, want, strings.Contains(art.Long, "scion artifact delete"))
			assert.Contains(t, names, "artifact.get")
		})
	}
}

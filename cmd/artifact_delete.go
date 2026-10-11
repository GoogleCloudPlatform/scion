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
	"io"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
)

// artifactDeleteCmd is "scion artifact delete". It is available in human
// mode only: it is deliberately not on the agent allow-list in
// cli_mode.go, and the hub refuses agents anyway.
var artifactDeleteCmd = &cobra.Command{
	Use:   "delete <ref>",
	Short: "Delete an artifact",
	Long: `Delete an artifact. Its grants and share links end with it, message
references to it show as unavailable, and its stored files are removed
after the hub's grace period. A deleted artifact cannot be restored.

The artifact's owner, a user it granted admin, or an admin of its home
project can delete it. Agents cannot delete artifacts.

The command asks for confirmation; pass --yes to skip the prompt (required
when stdin is not a terminal).

Examples:
  scion artifact delete scion://artifact/5f1c2d3e-...
  scion artifact delete scion://artifact/5f1c2d3e-... --yes`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return deleteArtifact(ctx, client.Artifacts(), cmd.OutOrStdout(), args[0], func(prompt string) bool {
			return hubsync.ConfirmAction(prompt, false, autoConfirm)
		})
	},
}

func init() {
	artifactCmd.AddCommand(artifactDeleteCmd)
}

// deleteArtifact runs scion artifact delete. confirm asks the user; a
// declined prompt deletes nothing and is not an error.
func deleteArtifact(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, ref string, confirm func(string) bool) error {
	id, seq, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	if seq != 0 {
		return fmt.Errorf("delete removes the whole artifact; give its reference without @%d", seq)
	}
	if !confirm(fmt.Sprintf("Delete artifact %s and end its grants and share links?", artifacts.FormatRef(id, 0))) {
		_, _ = fmt.Fprintln(out, "Not deleted.")
		return nil
	}
	if err := svc.Delete(ctx, id); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Deleted %s.\n", artifacts.FormatRef(id, 0))
	return nil
}

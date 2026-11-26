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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var (
	artifactTransferTo       string
	artifactTransferClearKey bool
)

// artifactTransferCmd is "scion artifact transfer". It is available in human
// mode only: it is deliberately not on the agent allow-list in cli_mode.go,
// and the hub refuses agents anyway.
var artifactTransferCmd = &cobra.Command{
	Use:   "transfer <ref> --to <agent>",
	Short: "Transfer ownership of an agent-owned artifact to another agent",
	Long: `Transfer ownership of an agent-owned artifact to another agent you control
in the same project. History, shares, grants, links and review state survive;
the new owner can publish versions and receives review notices.

Only a steward of an agent-owned artifact (a project owner, super-admin, or
the owning agent's delegating user) using an interactive session or dev
credential may transfer it. Transferring from a live agent also requires
that you control that agent.

The --to flag accepts an agent id, slug or name and is resolved only against
the hub's eligible transfer targets for the artifact.

Examples:
  scion artifact transfer scion://artifact/5f1c2d3e-... --to my-agent
  scion artifact transfer scion://artifact/5f1c2d3e-... --to my-agent --clear-key --yes`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(artifactTransferTo) == "" {
			return errors.New("--to <agent> is required")
		}
		_, client, _, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return transferArtifact(ctx, client.Artifacts(), cmd.OutOrStdout(), args[0], transferOptions{
			To:          artifactTransferTo,
			ClearKey:    artifactTransferClearKey,
			AsJSON:      isJSONOutput(),
			Yes:         autoConfirm,
			Interactive: util.IsTerminal(),
			Ask:         func(prompt string) bool { return hubsync.ConfirmAction(prompt, false, false) },
		})
	},
}

func init() {
	artifactTransferCmd.Flags().StringVar(&artifactTransferTo, "to", "", "Target agent (id, slug, or name)")
	artifactTransferCmd.Flags().BoolVar(&artifactTransferClearKey, "clear-key", false, "Clear the artifact's key if the target agent already owns a live artifact with the same key")
	artifactCmd.AddCommand(artifactTransferCmd)
}

var errTransferNeedsConfirmation = errors.New("not transferred: transferring ownership needs confirmation, and stdin is not a terminal; " +
	"pass --yes (or --non-interactive, or the cli.interactive_disabled setting, which imply it) to confirm")

type transferOptions struct {
	To          string
	ClearKey    bool
	AsJSON      bool
	Yes         bool
	Interactive bool
	Ask         func(prompt string) bool
}

func transferArtifact(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, ref string, opts transferOptions) error {
	id, seq, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	if seq != 0 {
		return fmt.Errorf("transfer applies to the whole artifact; give its reference without @%d", seq)
	}
	if strings.TrimSpace(opts.To) == "" {
		return errors.New("--to <agent> is required")
	}

	artResp, err := svc.Get(ctx, id)
	if err != nil {
		return err
	}
	art := artResp.Artifact
	if art.OwnerKind != artifacts.PrincipalKindAgent {
		return fmt.Errorf("artifact %s is owned by %s:%s; only agent-owned artifacts can be transferred",
			artifacts.FormatRef(id, 0), art.OwnerKind, art.OwnerRef)
	}

	targetsResp, err := svc.TransferTargets(ctx, id)
	if err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return errors.New("transfer is not enabled on this hub")
		}
		return err
	}

	target, err := resolveTransferTarget(opts.To, targetsResp.Agents)
	if err != nil {
		return err
	}

	ownerState := "live"
	if art.OwnerDeleted {
		ownerState = "deleted"
	}
	targetLabel := target.Slug
	if targetLabel == "" {
		targetLabel = target.ID
	}

	if !opts.Yes {
		if !opts.Interactive {
			return errTransferNeedsConfirmation
		}
		prompt := fmt.Sprintf("Transfer %q from agent %s (%s) to agent %s?",
			art.Title, art.OwnerRef, ownerState, targetLabel)
		if !opts.Ask(prompt) {
			_, _ = fmt.Fprintln(out, "Not transferred.")
			return nil
		}
	}

	res, err := svc.Transfer(ctx, id, &hubclient.ArtifactTransferRequest{
		To:          artifacts.PrincipalRef(artifacts.PrincipalKindAgent, target.ID),
		ExpectOwner: artifacts.PrincipalRef(art.OwnerKind, art.OwnerRef),
		ClearKey:    opts.ClearKey,
	})
	if err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) {
			switch {
			case apiErr.StatusCode == http.StatusNotFound:
				return errors.New("transfer is not enabled on this hub")
			case apiErr.StatusCode == http.StatusConflict && apiErr.Code == "key_conflict":
				return fmt.Errorf("key conflict: agent %s already owns a live artifact with key %q in this project; re-run with --clear-key to remove the key and transfer",
					targetLabel, art.Key)
			}
		}
		return err
	}

	if opts.AsJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	_, _ = fmt.Fprintf(out, "Transferred %s (%q) to agent %s (%s).\n",
		artifacts.FormatRef(id, 0), res.Artifact.Artifact.Title, targetLabel, target.ID)
	return nil
}

// resolveTransferTarget matches query against eligible transfer targets by
// ID, slug, or name. It never consults any other agent lookup.
func resolveTransferTarget(query string, candidates []hubclient.ArtifactTransferCandidate) (*hubclient.ArtifactTransferCandidate, error) {
	q := strings.TrimSpace(query)
	q = strings.TrimPrefix(q, "agent:")
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no eligible transfer targets found for %q", query)
	}
	var exact []hubclient.ArtifactTransferCandidate
	for _, c := range candidates {
		if strings.EqualFold(c.ID, q) || c.Slug == q || c.Name == q {
			exact = append(exact, c)
		}
	}
	if len(exact) == 1 {
		return &exact[0], nil
	}
	if len(exact) > 1 {
		return nil, fmt.Errorf("target %q is ambiguous; matching eligible agents: %s", query, formatCandidateList(exact))
	}
	// Fallback to case-insensitive slug/name match.
	var ci []hubclient.ArtifactTransferCandidate
	for _, c := range candidates {
		if strings.EqualFold(c.Slug, q) || strings.EqualFold(c.Name, q) {
			ci = append(ci, c)
		}
	}
	if len(ci) == 1 {
		return &ci[0], nil
	}
	if len(ci) > 1 {
		return nil, fmt.Errorf("target %q is ambiguous; matching eligible agents: %s", query, formatCandidateList(ci))
	}
	return nil, fmt.Errorf("target agent %q is not an eligible transfer target; eligible agents: %s",
		query, formatCandidateList(candidates))
}

func formatCandidateList(candidates []hubclient.ArtifactTransferCandidate) string {
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		label := c.Slug
		if label == "" {
			label = c.Name
		}
		if label != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", label, c.ID))
		} else {
			parts = append(parts, c.ID)
		}
	}
	return strings.Join(parts, ", ")
}

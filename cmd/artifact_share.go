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
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	artifactShareTTL    string
	artifactShareList   bool
	artifactShareRevoke string
)

var artifactShareCmd = &cobra.Command{
	Use:   "share <ref>",
	Short: "Create, list or revoke an artifact's share links",
	Long: `Create a share link for an artifact and print it. Anyone holding the
link can open the artifact's current version in a browser without signing
in, until the link expires or is revoked. The link is shown only once.

Only the artifact's owner, or a user it granted admin, can share it.
Agents cannot create share links.

Examples:
  scion artifact share scion://artifact/5f1c2d3e-...             # default lifetime (7 days unless the hub says otherwise)
  scion artifact share scion://artifact/5f1c2d3e-... --ttl 24h
  scion artifact share scion://artifact/5f1c2d3e-... --ttl 30d
  scion artifact share scion://artifact/5f1c2d3e-... --list
  scion artifact share scion://artifact/5f1c2d3e-... --revoke <link-id>`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, client, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return shareArtifact(ctx, client.Artifacts(), cmd.OutOrStdout(), GetHubEndpoint(settings), args[0],
			artifactShareTTL, artifactShareList, artifactShareRevoke)
	},
}

func init() {
	artifactShareCmd.Flags().StringVar(&artifactShareTTL, "ttl", "", "Link lifetime in whole hours or days, e.g. 24h or 7d (default: the hub's)")
	artifactShareCmd.Flags().BoolVar(&artifactShareList, "list", false, "List the artifact's active share links instead of creating one")
	artifactShareCmd.Flags().StringVar(&artifactShareRevoke, "revoke", "", "Revoke the share link with this id instead of creating one")
	artifactCmd.AddCommand(artifactShareCmd)
}

// parseShareTTL parses a link lifetime: whole hours ("24h") or days
// ("7d"), returned in hours; "" is 0 (the hub's default).
func parseShareTTL(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	unit := v[len(v)-1]
	n, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || n <= 0 || (unit != 'h' && unit != 'd') {
		return 0, fmt.Errorf("--ttl must be a positive number of hours or days, such as 24h or 7d")
	}
	if unit == 'd' {
		if n > 1<<20 {
			return 0, fmt.Errorf("--ttl is too long")
		}
		n *= 24
	}
	return n, nil
}

// shareArtifact runs scion artifact share.
func shareArtifact(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, hubEndpoint, ref, ttl string, list bool, revoke string) error {
	id, _, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	switch {
	case list && revoke != "":
		return fmt.Errorf("--list and --revoke cannot be combined")
	case (list || revoke != "") && ttl != "":
		return fmt.Errorf("--ttl applies only when creating a link")
	case list:
		links, err := svc.ListLinks(ctx, id)
		if err != nil {
			return err
		}
		if len(links) == 0 {
			_, _ = fmt.Fprintln(out, "No active share links.")
			return nil
		}
		for _, l := range links {
			_, _ = fmt.Fprintf(out, "%s  created %s by %s  expires %s\n", l.ID,
				clitime.Format(l.CreatedAt, clitime.Minute), l.CreatedBy, clitime.Format(l.ExpiresAt, clitime.Minute))
		}
		return nil
	case revoke != "":
		if err := svc.RevokeLink(ctx, id, revoke); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "Revoked share link %s.\n", revoke)
		return nil
	}
	hours, err := parseShareTTL(ttl)
	if err != nil {
		return err
	}
	created, err := svc.CreateLink(ctx, id, hours)
	if err != nil {
		return err
	}
	link := created.URL
	if hubEndpoint != "" {
		link = strings.TrimSuffix(hubEndpoint, "/") + created.URL
	}
	_, _ = fmt.Fprintln(out, link)
	_, _ = fmt.Fprintf(out, "Expires %s. This link is shown only once; anyone holding it can open the artifact.\n",
		clitime.Format(created.Link.ExpiresAt, clitime.Minute))
	if created.ClampedToArtifactExpiry {
		_, _ = fmt.Fprintln(out, "The link ends early, when the artifact itself expires.")
	}
	_, _ = fmt.Fprintf(out, "Revoke it with: scion artifact share %s --revoke %s\n", artifacts.FormatRef(id, 0), created.Link.ID)
	return nil
}

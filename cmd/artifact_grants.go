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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	artifactGrantsAdd        string
	artifactGrantsPermission string
	artifactGrantsRemove     string
)

var artifactGrantsCmd = &cobra.Command{
	Use:   "grants <ref>",
	Short: "List, add or remove an artifact's grants (users only)",
	Long: `List the grants of an artifact, or add, change or remove one.

A grant gives a user, an agent or a project read, write or admin access to
the artifact; a user can also be given review (publish reviews only).
Only the artifact's owner, or a user it granted admin, can see or change
its grants. On an artifact owned by an agent, the user who delegated to
the agent and the home project's owners and admins (and super-admins)
may also add a review grant for a user who can already read it, and
nothing else; they cannot list the grants. Agents cannot manage grants.

Subjects are written user:<id>, agent:<id> or project:<id>. Adding a grant
for a subject that already has one changes its permission.

Examples:
  scion artifact grants scion://artifact/5f1c2d3e-...
  scion artifact grants scion://artifact/5f1c2d3e-... --add user:<id> --permission read
  scion artifact grants scion://artifact/5f1c2d3e-... --add project:<id> --permission write
  scion artifact grants scion://artifact/5f1c2d3e-... --add user:<id> --permission review
  scion artifact grants scion://artifact/5f1c2d3e-... --remove <grant-id>`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return artifactGrants(ctx, client.Artifacts(), cmd.OutOrStdout(), args[0], grantsOptions{
			Add:        artifactGrantsAdd,
			Permission: artifactGrantsPermission,
			Remove:     artifactGrantsRemove,
			JSON:       isJSONOutput(),
		})
	},
}

func init() {
	artifactGrantsCmd.Flags().StringVar(&artifactGrantsAdd, "add", "", "Add or change the grant of this subject (user:<id>, agent:<id> or project:<id>)")
	artifactGrantsCmd.Flags().StringVar(&artifactGrantsPermission, "permission", "", "Permission for --add: read, review (users only), write or admin")
	artifactGrantsCmd.Flags().StringVar(&artifactGrantsRemove, "remove", "", "Remove the grant with this id")
	artifactCmd.AddCommand(artifactGrantsCmd)
}

// grantsOptions are the flags of artifact grants.
type grantsOptions struct {
	Add        string
	Permission string
	Remove     string
	JSON       bool
}

// grantPermissions are the permissions a grant can carry.
var grantPermissions = []string{"read", "review", "write", "admin"}

// parseGrantSubject maps user:<id>, agent:<id> and project:<id> to the
// hub's subjectKind and subjectRef.
func parseGrantSubject(s string) (kind, ref string, err error) {
	prefix, id, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || strings.TrimSpace(id) == "" {
		return "", "", fmt.Errorf("subject %q must be user:<id>, agent:<id> or project:<id>", s)
	}
	id = strings.TrimSpace(id)
	switch strings.ToLower(prefix) {
	case "user":
		return "principal", "user:" + id, nil
	case "agent":
		return "principal", "agent:" + id, nil
	case "project":
		return "scope", id, nil
	}
	return "", "", fmt.Errorf("subject %q must be user:<id>, agent:<id> or project:<id>", s)
}

// grantSubjectText is the subject of a grant as the CLI writes it.
func grantSubjectText(g hubclient.ArtifactGrant) string {
	if g.SubjectKind == "scope" {
		return "project:" + g.SubjectRef
	}
	return g.SubjectRef
}

// artifactGrants lists the grants of the artifact ref names, or adds,
// changes or removes one.
func artifactGrants(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, ref string, opts grantsOptions) error {
	id, _, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	removeSet := opts.Remove != ""
	opts.Remove = strings.TrimSpace(opts.Remove)
	if removeSet && opts.Remove == "" {
		return errors.New("--remove needs a grant id")
	}
	switch {
	case opts.Add != "" && opts.Remove != "":
		return errors.New("--add and --remove cannot be used together")
	case opts.Permission != "" && opts.Add == "":
		return errors.New("--permission goes with --add")
	case opts.Add != "":
		kind, subject, err := parseGrantSubject(opts.Add)
		if err != nil {
			return err
		}
		perm := strings.ToLower(strings.TrimSpace(opts.Permission))
		if !containsString(grantPermissions, perm) {
			return fmt.Errorf("--permission must be one of %s", strings.Join(grantPermissions, ", "))
		}
		g, err := svc.PutGrant(ctx, id, &hubclient.ArtifactGrantRequest{SubjectKind: kind, SubjectRef: subject, Permission: perm})
		if err != nil {
			return fmt.Errorf("add grant: %w%s", err, grantsErrorHint(err))
		}
		if opts.JSON {
			return writeJSONTo(out, g)
		}
		_, _ = fmt.Fprintf(out, "Granted %s %s on %s (grant %s)\n", safeText(grantSubjectText(*g)), safeText(g.Permission), artifacts.FormatRef(id, 0), safeText(g.ID))
		return nil
	case opts.Remove != "":
		if err := svc.DeleteGrant(ctx, id, opts.Remove); err != nil {
			return fmt.Errorf("remove grant: %w%s", err, grantsErrorHint(err))
		}
		if opts.JSON {
			return writeJSONTo(out, map[string]string{"removed": opts.Remove})
		}
		_, _ = fmt.Fprintf(out, "Removed grant %s from %s\n", safeText(opts.Remove), artifacts.FormatRef(id, 0))
		return nil
	}
	list, err := svc.ListGrants(ctx, id)
	if err != nil {
		return fmt.Errorf("list grants: %w%s", err, grantsErrorHint(err))
	}
	if opts.JSON {
		if list.Grants == nil {
			list.Grants = []hubclient.ArtifactGrant{}
		}
		return writeJSONTo(out, list)
	}
	if len(list.Grants) == 0 {
		_, _ = fmt.Fprintf(out, "%s has no grants\n", artifacts.FormatRef(id, 0))
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSUBJECT\tPERMISSION\tCREATED")
	for _, g := range list.Grants {
		subject := grantSubjectText(g)
		if g.Home {
			subject += " (home project)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", safeText(g.ID), safeText(subject), safeText(g.Permission), clitime.Format(g.CreatedAt, clitime.Minute))
	}
	return tw.Flush()
}

// grantsErrorHint explains the grants routes' refusals: they serve the
// artifact's administrators only, and answer 403 to other readers of the
// artifact and 404 to non-readers.
func grantsErrorHint(err error) string {
	if h := artifactErrorHint(err, false); h != "" {
		return h + "\nOnly the artifact's owner, or a user it granted admin, can see or change its grants."
	}
	return ""
}

// writeJSONTo writes v to out as indented JSON.
func writeJSONTo(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// containsString reports whether list holds s.
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

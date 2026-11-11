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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// scion hub test-identity cleanup and provenance (ptone/scion#4240, Phase
// 3c). cleanup asks the hub to remove what the caller's expired or deleted
// test identities created; the hub selects the targets from the provenance
// it recorded at creation, the command names none. provenance asks the hub
// whether one resource was recorded as created under a test identity.
//
// Both follow the test-identity profile gate (hub_profile.go): human mode
// with or without a hub profile, and agent mode only with a target-hub
// profile for another hub (an agent runner verifying a run by absence on a
// target hub).

// errTestIdentityCleanupForbidden is returned when the hub refuses the
// caller (403): it needs test_identity.issue, and test identities of
// every kind, the admin roles included, are refused.
var errTestIdentityCleanupForbidden = errors.New("the hub refused the caller: this needs the test_identity.issue permission (an issuer session or issuer token, or the profile's issuer delegation); test identities, the admin roles included, never clean up")

// errTestIdentitiesOff is returned when the hub answers 404: test
// identities are not enabled there.
var errTestIdentitiesOff = errors.New("test identities are not enabled on this hub (it must run with --test-mode=member or above)")

// errExpectAbsentResiduals is returned when an --expect-absent check left
// residuals: unproven IDs (a real administrator must decide) or proven IDs
// that were not removed.
var errExpectAbsentResiduals = errors.New("some expected-absent resources are still present or unproven")

var hubTestIdentityCleanupCmd = &cobra.Command{
	Use:   "cleanup [--dry-run] [--expect-absent FILE]",
	Short: "Remove what your expired or deleted test identities created",
	Long: `Remove the agents, schedules, groups and projects that your expired or
deleted test identities created, and the expired identities themselves.

The hub selects the targets: only resources it recorded, at creation, as
created under a test identity you issued (with a hub profile: issued
through the profile's issuer delegation), and only for identities that have
expired or been deleted. A live identity is never touched, nor is any
resource of another issuer. An admin session reaches every issuer. Nothing
is selected by name, label, owner or creator. A project that has another
owner, or resources from someone else, is kept.

--dry-run lists what a run would remove and changes nothing.

--expect-absent FILE checks a list of resources the run expects to be gone,
one "<type>:<id>" per line (type agent, project, schedule or group; blank
lines and lines starting with # are ignored). Before the cleanup, each one
is checked with the hub's provenance record. A proven one is reported as
cleaned once the run removed it. One that is already gone (the expiry
reaper, a purge or an earlier cleanup removed it) is reported as absent,
proven by its deletion record. An unproven one (the hub has no record, in
your issuer scope, that a test identity created or deleted it; for example
a resource from before provenance recording) is never removed: it is
printed with proven:false, the reason and "real-admin action required".
The command exits non-zero when any expected resource is unproven or
still present.

A run that cannot finish (for example a broker is offline) lists what is
left and exits non-zero; run it again to finish.

Examples:
  scion hub test-identity cleanup --dry-run
  scion hub test-identity cleanup
  scion hub test-identity cleanup --expect-absent ./created-ids.txt
  scion --hub-profile t1 hub test-identity cleanup --expect-absent ./created-ids.txt`,
	Args: cobra.NoArgs,
	RunE: runHubTestIdentityCleanup,
}

var hubTestIdentityProvenanceCmd = &cobra.Command{
	Use:   "provenance TYPE:ID",
	Short: "Show whether a resource was recorded as created by a test identity",
	Long: `Ask the hub whether it recorded one resource as created under a test
identity. TYPE is agent, project, schedule or group.

The answer covers your issuer scope only: test identities you issued
(with a hub profile, those issued through the profile's issuer
delegation), or every issuer's for an admin sign-in. A proven resource
shows the test identity it is recorded under, that identity's issuer and
its state (live, expired or absent). 'cleanup' removes it once its identity
has expired or been deleted. A resource that is already gone is proven by
its deletion record, whose correlation ID is shown. An unproven resource
shows only the reason; the hub never reports its owner or creator, and a
resource that does not exist, or another issuer's, gets the same answer.
An unproven resource is never removed by cleanup: a real administrator
must decide.

Examples:
  scion hub test-identity provenance agent:6f1c...
  scion hub test-identity provenance project:0b2d... --json`,
	Args: cobra.ExactArgs(1),
	RunE: runHubTestIdentityProvenance,
}

var (
	testIdentityCleanupDryRun       bool
	testIdentityCleanupExpectAbsent string
)

func init() {
	hubTestIdentityCmd.AddCommand(hubTestIdentityCleanupCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityProvenanceCmd)
	hubTestIdentityCleanupCmd.Flags().BoolVar(&testIdentityCleanupDryRun, "dry-run", false, "List what would be removed; change nothing")
	hubTestIdentityCleanupCmd.Flags().StringVar(&testIdentityCleanupExpectAbsent, "expect-absent", "", `File of "<type>:<id>" lines expected to be gone after the cleanup`)
	hubTestIdentityCleanupCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Print the report as JSON")
	hubTestIdentityProvenanceCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Print the answer as JSON")
}

func runHubTestIdentityCleanup(cmd *cobra.Command, _ []string) error {
	var expected []string
	if testIdentityCleanupExpectAbsent != "" {
		var err error
		if expected, err = readExpectAbsent(testIdentityCleanupExpectAbsent); err != nil {
			return err
		}
	}
	client, err := testIdentityIssuerClient()
	if err != nil {
		return err
	}
	// A cleanup waits for each agent's delete on its broker.
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
	defer cancel()
	return cleanupTestIdentities(ctx, cmd.OutOrStdout(), client.TestIdentities(), testIdentityCleanupOptions{
		DryRun: testIdentityCleanupDryRun, ExpectAbsent: expected, JSON: testIdentityJSON,
	})
}

func runHubTestIdentityProvenance(cmd *cobra.Command, args []string) error {
	resource, err := parseProvenanceResource(args[0])
	if err != nil {
		return newUsageError("%s", err.Error())
	}
	client, err := testIdentityIssuerClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return showTestIdentityProvenance(ctx, cmd.OutOrStdout(), client.TestIdentities(), resource, testIdentityJSON)
}

// provenanceResourceTypes are the resource types the hub records
// provenance for.
var provenanceResourceTypes = map[string]bool{"agent": true, "project": true, "schedule": true, "group": true}

// parseProvenanceResource checks a "<type>:<id>" reference and returns it
// trimmed.
func parseProvenanceResource(s string) (string, error) {
	s = strings.TrimSpace(s)
	typ, id, ok := strings.Cut(s, ":")
	if !ok || id == "" || strings.ContainsAny(id, "/: \t") {
		return "", fmt.Errorf("invalid resource %q: want <type>:<id>, type one of agent, project, schedule, group", s)
	}
	if !provenanceResourceTypes[typ] {
		return "", fmt.Errorf("invalid resource type %q in %q: want agent, project, schedule or group", typ, s)
	}
	return s, nil
}

// readExpectAbsent reads an --expect-absent file: one "<type>:<id>" per
// line, blank lines and # comments ignored, duplicates dropped (first kept).
// An invalid line refuses the whole file before anything is sent.
func readExpectAbsent(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--expect-absent: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		res, err := parseProvenanceResource(text)
		if err != nil {
			return nil, newUsageError("--expect-absent %s line %d: %v", path, line, err)
		}
		if !seen[res] {
			seen[res] = true
			out = append(out, res)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("--expect-absent: %w", err)
	}
	return out, nil
}

// testIdentityCleanupOptions are the inputs of cleanupTestIdentities.
type testIdentityCleanupOptions struct {
	DryRun       bool
	ExpectAbsent []string
	JSON         bool
}

// expectAbsentResult is the outcome of one --expect-absent resource.
type expectAbsentResult struct {
	Resource string `json:"resource"`
	Proven   bool   `json:"proven"`
	// Outcome is cleaned, would_clean (dry run), absent, not_cleaned or
	// unproven.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// FixtureRootID and FixtureState are set for a proven resource.
	FixtureRootID string `json:"fixtureRootId,omitempty"`
	FixtureState  string `json:"fixtureState,omitempty"`
	// DeletionRecord is the correlation ID of the deletion record that
	// proves an absent resource.
	DeletionRecord string `json:"deletionRecord,omitempty"`
}

// Outcomes of an --expect-absent resource.
const (
	expectAbsentCleaned    = "cleaned"
	expectAbsentWouldClean = "would_clean"
	expectAbsentAbsent     = "absent"
	expectAbsentNotCleaned = "not_cleaned"
	expectAbsentUnproven   = "unproven"
)

// cleanupTestIdentities runs (or dry-runs) the cleanup and, with
// ExpectAbsent, checks each expected resource: its provenance before the
// run, then the run's targets (and, for a proven resource the run did not
// select, its provenance after the run). It returns an error when the run
// did not complete or any expected resource is unproven or still present.
func cleanupTestIdentities(ctx context.Context, out io.Writer, svc hubclient.TestIdentityService, opts testIdentityCleanupOptions) error {
	before := make([]*hubclient.TestIdentityProvenance, len(opts.ExpectAbsent))
	for i, res := range opts.ExpectAbsent {
		p, err := svc.Provenance(ctx, res)
		if err != nil {
			return testIdentityCleanupError("check provenance of "+res, err)
		}
		before[i] = p
	}

	report, runErr := svc.Cleanup(ctx, opts.DryRun)
	if runErr != nil && report == nil {
		return testIdentityCleanupError("clean up test identities", runErr)
	}

	var results []expectAbsentResult
	residuals := 0
	for i, res := range opts.ExpectAbsent {
		r := expectAbsentResult{Resource: res, Proven: before[i].Proven}
		switch {
		case !before[i].Proven:
			r.Outcome, r.Reason = expectAbsentUnproven, before[i].Reason
		case before[i].Deleted:
			// Already gone, proven by its deletion record (the reaper, a
			// purge or an earlier cleanup removed it).
			r.FixtureRootID, r.FixtureState = before[i].FixtureRootID, before[i].FixtureState
			r.Outcome, r.DeletionRecord = expectAbsentAbsent, before[i].DeletionRecord
		default:
			r.FixtureRootID, r.FixtureState = before[i].FixtureRootID, before[i].FixtureState
			r.Outcome, r.Reason = settleExpectAbsent(ctx, svc, report, res, opts.DryRun)
			if r.Outcome == expectAbsentAbsent {
				// settleExpectAbsent returns the deletion record, if any,
				// as the reason of an absent outcome.
				r.DeletionRecord, r.Reason = r.Reason, ""
			}
		}
		if r.Outcome == expectAbsentUnproven || r.Outcome == expectAbsentNotCleaned {
			residuals++
		}
		results = append(results, r)
	}

	if opts.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Report       *hubclient.TestIdentityCleanupReport `json:"report"`
			ExpectAbsent []expectAbsentResult                 `json:"expectAbsent,omitempty"`
		}{report, results}); err != nil {
			return err
		}
	} else {
		printTestIdentityCleanupReport(out, report)
		printExpectAbsent(out, results)
	}

	if runErr != nil {
		return fmt.Errorf("test identity cleanup did not complete; run the command again to finish")
	}
	if residuals > 0 {
		return fmt.Errorf("%w: %d of %d", errExpectAbsentResiduals, residuals, len(results))
	}
	return nil
}

// settleExpectAbsent returns the outcome of a proven expected resource
// from the run's targets: cleaned (would_clean for a dry run) when the run
// removed it, not_cleaned when it selected it but did not remove it.
// When the run did not select it, a dry run reports not_cleaned; a real
// run asks the hub again: gone with an in-scope deletion record is absent,
// and the second value is then that record's correlation ID; gone without
// one (no longer proven) is absent with an empty second value; still
// present is not_cleaned with its identity's state. For every other
// outcome the second value is the reason.
func settleExpectAbsent(ctx context.Context, svc hubclient.TestIdentityService, report *hubclient.TestIdentityCleanupReport, res string, dryRun bool) (string, string) {
	typ, id, _ := strings.Cut(res, ":")
	for _, t := range report.Targets {
		if t.Type != typ || t.ID != id {
			continue
		}
		switch t.Status {
		case "deleted":
			return expectAbsentCleaned, ""
		case "target":
			return expectAbsentWouldClean, ""
		default:
			reason := t.Status
			if t.Reason != "" {
				reason += ": " + t.Reason
			}
			return expectAbsentNotCleaned, reason
		}
	}
	if dryRun {
		return expectAbsentNotCleaned, "not selected by this cleanup (its test identity is live, or another issuer's)"
	}
	after, err := svc.Provenance(ctx, res)
	if err != nil {
		return expectAbsentNotCleaned, "provenance check after the cleanup failed"
	}
	if after.Proven && after.Deleted {
		return expectAbsentAbsent, after.DeletionRecord
	}
	if !after.Proven {
		return expectAbsentAbsent, ""
	}
	return expectAbsentNotCleaned, fmt.Sprintf("still present; its test identity is %s (cleanup removes only an expired or deleted identity's resources, and only for its issuer)", after.FixtureState)
}

func printTestIdentityCleanupReport(out io.Writer, r *hubclient.TestIdentityCleanupReport) {
	d := r.Deleted
	if r.DryRun {
		_, _ = fmt.Fprintf(out, "Cleanup dry run (%s scope): %d test identities, %d targets; nothing was changed.\n", r.Scope, len(r.Identities), len(r.Targets))
	} else {
		_, _ = fmt.Fprintf(out, "Cleanup (%s scope, run %s): removed %d agents, %d schedules, %d groups, %d projects and %d test identities.\n",
			r.Scope, r.RunID, d.Agents, d.Schedules, d.Groups, d.Projects, r.IdentitiesDeleted)
	}
	for _, id := range r.Identities {
		line := fmt.Sprintf("  test identity %s (%s)", id.ID, id.State)
		if id.Error != "" {
			line += ": " + id.Error
		}
		_, _ = fmt.Fprintln(out, line)
	}
	for _, t := range r.Targets {
		line := fmt.Sprintf("    %s %s %s", t.Status, t.Type, t.ID)
		if t.Reason != "" {
			line += " (" + t.Reason + ")"
		}
		_, _ = fmt.Fprintln(out, line)
	}
	for _, it := range r.Unproven {
		_, _ = fmt.Fprintf(out, "  unproven: %s %s in project %s (not recorded as created by a test identity; a real administrator must decide)\n", it.Type, it.ID, it.ProjectID)
	}
	if r.LiveSkipped > 0 {
		_, _ = fmt.Fprintf(out, "  %d live test identities were not touched\n", r.LiveSkipped)
	}
	if r.Truncated {
		_, _ = fmt.Fprintln(out, "  more remains than one run handles; run the command again")
	}
}

func printExpectAbsent(out io.Writer, results []expectAbsentResult) {
	if len(results) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "Expected absent:")
	for _, r := range results {
		switch r.Outcome {
		case expectAbsentUnproven:
			_, _ = fmt.Fprintf(out, "  %s: proven:false, reason: %s; real-admin action required (never removed by cleanup)\n", r.Resource, r.Reason)
		case expectAbsentCleaned:
			_, _ = fmt.Fprintf(out, "  %s: cleaned (proven, test identity %s)\n", r.Resource, r.FixtureRootID)
		case expectAbsentWouldClean:
			_, _ = fmt.Fprintf(out, "  %s: would be cleaned (proven, test identity %s)\n", r.Resource, r.FixtureRootID)
		case expectAbsentAbsent:
			if r.DeletionRecord != "" {
				_, _ = fmt.Fprintf(out, "  %s: absent (proven by deletion record %s)\n", r.Resource, r.DeletionRecord)
			} else {
				_, _ = fmt.Fprintf(out, "  %s: absent (proven before the run, gone after it)\n", r.Resource)
			}
		default:
			_, _ = fmt.Fprintf(out, "  %s: NOT cleaned (proven, test identity %s): %s\n", r.Resource, r.FixtureRootID, r.Reason)
		}
	}
}

// showTestIdentityProvenance prints the hub's provenance answer for one
// resource.
func showTestIdentityProvenance(ctx context.Context, out io.Writer, svc hubclient.TestIdentityService, resource string, asJSON bool) error {
	p, err := svc.Provenance(ctx, resource)
	if err != nil {
		return testIdentityCleanupError("check provenance of "+resource, err)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	if !p.Proven {
		_, _ = fmt.Fprintf(out, "%s: proven:false, reason: %s\n", p.Resource, p.Reason)
		_, _ = fmt.Fprintln(out, "Cleanup never removes it; a real administrator must decide.")
		return nil
	}
	if p.Deleted {
		_, _ = fmt.Fprintf(out, "%s: proven:true, deleted (deletion record %s), test identity %s (%s), issuer %s\n", p.Resource, p.DeletionRecord, p.FixtureRootID, p.FixtureState, p.FixtureIssuerID)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s: proven:true, test identity %s (%s), issuer %s\n", p.Resource, p.FixtureRootID, p.FixtureState, p.FixtureIssuerID)
	return nil
}

// testIdentityCleanupError maps the hub's refusals to named errors.
func testIdentityCleanupError(what string, err error) error {
	switch {
	case apiclient.IsForbiddenError(err) || apiclient.IsUnauthorizedError(err):
		return fmt.Errorf("%s: %w", what, errTestIdentityCleanupForbidden)
	case apiclient.IsNotFoundError(err):
		return fmt.Errorf("%s: %w", what, errTestIdentitiesOff)
	}
	return fmt.Errorf("failed to %s: %w", what, err)
}

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

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	attrReportProject string
	attrReportDB      string
)

var serverAttributionReportCmd = &cobra.Command{
	Use:   "attribution-report",
	Short: "Report on conversation attribution completeness",
	Long: `Scan messages and report how many are attributed to a conversation,
how many can be backfilled, how many have non-UUID principals (and therefore
cannot be attributed), and how many are otherwise unresolvable.

This is a READ-ONLY command: it examines the database and reports findings
without modifying any rows.

A non-zero 'non-UUID principal' or 'unresolvable' count means the conversation
read switch CANNOT be safely enabled — those messages would become invisible.

Examples:
  # Report for all projects:
  scion server attribution-report

  # Report for a specific project:
  scion server attribution-report --project <project-id>`,
	RunE: runServerAttributionReport,
}

func init() {
	serverCmd.AddCommand(serverAttributionReportCmd)
	serverAttributionReportCmd.Flags().StringVar(&attrReportProject, "project", "", "Report for a specific project ID (default: all)")
	serverAttributionReportCmd.Flags().StringVar(&attrReportDB, "db", "", "Database DSN (overrides config/env)")
}

// AttributionReport holds the results of the attribution completeness scan.
type AttributionReport struct {
	// Total is the count of all messages examined.
	Total int
	// Attributed is the count of messages with a non-empty conversation_id.
	Attributed int
	// Backfillable is the count of unattributed messages whose sender and
	// recipient IDs are both valid UUIDs — these can be attributed by the
	// backfill command.
	Backfillable int
	// NonUUIDPrincipal is the count of unattributed messages where at least
	// one principal ID is not a valid UUID (e.g. federated identities,
	// slugs). Backfill cannot ever repair these rows because the information
	// needed to derive a DM key does not exist in the database.
	NonUUIDPrincipal int
	// Unresolvable is the count of unattributed messages whose principal IDs
	// are valid UUIDs but key derivation still fails or the row lacks the
	// inputs to derive at all.
	Unresolvable int

	// NonUUIDExamples holds the offending principal IDs for non-UUID
	// principal messages. IDs only — never message content.
	NonUUIDExamples []NonUUIDExample

	// UnresolvableExamples holds a small sample of unresolvable messages for
	// diagnosis. Each entry contains IDs and keys only — never message content.
	UnresolvableExamples []AttributionExample
}

// NonUUIDExample holds identifying information for a message whose principal
// is not a valid UUID. Contains IDs only — never message content.
type NonUUIDExample struct {
	MessageID   string
	SenderID    string
	RecipientID string
}

// AttributionExample holds identifying information for an unresolvable
// message. Never includes message content.
type AttributionExample struct {
	MessageID   string
	SenderID    string
	RecipientID string
	ThreadID    string
	DeriveError string
}

func runServerAttributionReport(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// Reuse the same store-opening logic as the backfill command.
	// Override the --db flag if provided.
	savedDB := backfillDB
	if attrReportDB != "" {
		backfillDB = attrReportDB
	}
	s, err := openBackfillStore(ctx)
	backfillDB = savedDB
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	// Determine which projects to report on.
	var projectIDs []string
	if attrReportProject != "" {
		projectIDs = []string{attrReportProject}
	} else {
		cursor := ""
		for {
			projects, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500, Cursor: cursor})
			if err != nil {
				return fmt.Errorf("listing projects: %w", err)
			}
			for _, p := range projects.Items {
				projectIDs = append(projectIDs, p.ID)
			}
			if projects.NextCursor == "" {
				break
			}
			cursor = projects.NextCursor
		}
		if len(projectIDs) == 0 {
			_, _ = fmt.Fprintln(out, "No projects found.")
			return nil
		}
	}

	// Aggregate results across all projects.
	total := &AttributionReport{}

	for _, pid := range projectIDs {
		result, err := runAttributionReportForProject(ctx, s, pid)
		if err != nil {
			return fmt.Errorf("attribution report for project %s: %w", pid, err)
		}
		mergeAttributionReport(total, result)
	}

	// Print report.
	projectLabel := attrReportProject
	if projectLabel == "" {
		projectLabel = fmt.Sprintf("ALL (%d projects)", len(projectIDs))
	}
	printAttributionReport(out, total, projectLabel)

	return nil
}

// runAttributionReportForProject scans all messages in a single project and
// classifies unattributed messages into buckets. This is the testable core.
//
// It is strictly read-only: it queries messages and attempts key derivation
// using the production DeriveConversationKey function, but never writes.
func runAttributionReportForProject(ctx context.Context, s store.Store, projectID string) (*AttributionReport, error) {
	report := &AttributionReport{}

	const batchSize = 500
	cursor := ""

	for {
		page, err := s.ListMessages(ctx, store.MessageFilter{
			ProjectID: projectID,
		}, store.ListOptions{
			Limit:  batchSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("listing messages: %w", err)
		}

		for i := range page.Items {
			msg := &page.Items[i]
			report.Total++

			// Already attributed — nothing to classify.
			if msg.ConversationID != "" {
				report.Attributed++
				continue
			}

			// Unattributed: classify into sub-buckets.
			classifyUnattributedMessage(report, msg, projectID)
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	return report, nil
}

// classifyUnattributedMessage determines which unattributed bucket a message
// belongs to, using the production key-derivation functions.
func classifyUnattributedMessage(report *AttributionReport, msg *store.Message, projectID string) {
	// Extract principal kind and ID, same as the backfill service.
	senderKind, senderID := parsePrincipalForReport(msg.Sender, msg.SenderID)
	recipientKind, recipientID := parsePrincipalForReport(msg.Recipient, msg.RecipientID)

	// Check whether both principal IDs are valid UUIDs.
	// A non-UUID principal means this message can NEVER be attributed by
	// backfill — DMConversationKey requires UUIDs on both sides.
	senderIsUUID := isUUID(senderID)
	recipientIsUUID := isUUID(recipientID)

	if !senderIsUUID || !recipientIsUUID {
		report.NonUUIDPrincipal++
		report.NonUUIDExamples = append(report.NonUUIDExamples, NonUUIDExample{
			MessageID:   msg.ID,
			SenderID:    senderID,
			RecipientID: recipientID,
		})
		return
	}

	// Both principals are UUIDs. Attempt key derivation using the production
	// function to see if it would succeed.
	_, _, _, deriveErr := messaging.DeriveConversationKey(messaging.KeyInputs{
		ThreadID:      msg.ThreadID,
		ProjectID:     projectID,
		SenderKind:    senderKind,
		SenderID:      senderID,
		RecipientKind: recipientKind,
		RecipientID:   recipientID,
	})

	if deriveErr == nil {
		report.Backfillable++
		return
	}

	// Derivation failed despite valid UUIDs — unresolvable.
	report.Unresolvable++
	if len(report.UnresolvableExamples) < 10 {
		report.UnresolvableExamples = append(report.UnresolvableExamples, AttributionExample{
			MessageID:   msg.ID,
			SenderID:    senderID,
			RecipientID: recipientID,
			ThreadID:    msg.ThreadID,
			DeriveError: deriveErr.Error(),
		})
	}
}

// parsePrincipalForReport extracts kind and ID from a message's sender/recipient
// fields. This mirrors the backfill's parsePrincipal logic exactly, ensuring
// the report classifies messages the same way backfill would process them.
func parsePrincipalForReport(label, id string) (kind, principalID string) {
	// Extract kind from the label prefix.
	if idx := indexByte(label, ':'); idx >= 0 {
		kind = label[:idx]
	} else {
		kind = "user" // default
	}

	// Prefer the explicit ID field; fall back to the name part of the label.
	if id != "" {
		principalID = id
	} else if idx := indexByte(label, ':'); idx >= 0 {
		principalID = label[idx+1:]
	} else {
		principalID = label
	}

	return kind, principalID
}

// indexByte returns the index of the first instance of c in s, or -1.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// isUUID reports whether s is a valid UUID.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// mergeAttributionReport adds the counts from src into dst.
func mergeAttributionReport(dst, src *AttributionReport) {
	dst.Total += src.Total
	dst.Attributed += src.Attributed
	dst.Backfillable += src.Backfillable
	dst.NonUUIDPrincipal += src.NonUUIDPrincipal
	dst.Unresolvable += src.Unresolvable
	dst.NonUUIDExamples = append(dst.NonUUIDExamples, src.NonUUIDExamples...)
	dst.UnresolvableExamples = append(dst.UnresolvableExamples, src.UnresolvableExamples...)
	if len(dst.UnresolvableExamples) > 10 {
		dst.UnresolvableExamples = dst.UnresolvableExamples[:10]
	}
}

// printAttributionReport writes the human-readable report to out.
func printAttributionReport(out io.Writer, r *AttributionReport, projectLabel string) {
	_, _ = fmt.Fprintf(out, "Attribution completeness — project %s\n", projectLabel)
	_, _ = fmt.Fprintf(out, "  messages total                        %d\n", r.Total)
	_, _ = fmt.Fprintf(out, "  attributed (conversation_id set)      %d\n", r.Attributed)
	_, _ = fmt.Fprintf(out, "  unattributed — backfillable           %d", r.Backfillable)
	if r.Backfillable > 0 {
		_, _ = fmt.Fprint(out, "   -> run 'scion server backfill --execute'")
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "  unattributed — non-UUID principal     %d", r.NonUUIDPrincipal)
	if r.NonUUIDPrincipal > 0 {
		_, _ = fmt.Fprint(out, "   -> BLOCKS FLIP (DEF-32)")
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "  unattributed — unresolvable           %d", r.Unresolvable)
	if r.Unresolvable > 0 {
		_, _ = fmt.Fprint(out, "   -> BLOCKS FLIP, examples below")
	}
	_, _ = fmt.Fprintln(out)

	// Flip-blocking summary.
	if r.NonUUIDPrincipal > 0 || r.Unresolvable > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "*** FLIP BLOCKED ***")
		if r.NonUUIDPrincipal > 0 {
			_, _ = fmt.Fprintf(out, "  %d message(s) have non-UUID principal IDs and cannot be attributed.\n", r.NonUUIDPrincipal)
			_, _ = fmt.Fprintln(out, "  These are permanently unattributable without a federated identity link table (DEF-32).")
		}
		if r.Unresolvable > 0 {
			_, _ = fmt.Fprintf(out, "  %d message(s) have valid UUID principals but key derivation fails.\n", r.Unresolvable)
		}
		_, _ = fmt.Fprintln(out, "  The conversation read switch MUST NOT be enabled until these are resolved.")
	}

	// Print non-UUID principal IDs.
	if len(r.NonUUIDExamples) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Non-UUID principal IDs (IDs only, no message content):")
		for _, ex := range r.NonUUIDExamples {
			_, _ = fmt.Fprintf(out, "  message=%s sender=%s recipient=%s\n",
				ex.MessageID, ex.SenderID, ex.RecipientID)
		}
	}

	// Print examples for unresolvable messages.
	if len(r.UnresolvableExamples) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Unresolvable examples (IDs and keys only, no message content):")
		for _, ex := range r.UnresolvableExamples {
			_, _ = fmt.Fprintf(out, "  message=%s sender=%s recipient=%s thread=%q error=%q\n",
				ex.MessageID, ex.SenderID, ex.RecipientID, ex.ThreadID, ex.DeriveError)
		}
	}
}

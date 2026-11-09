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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/critic"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var artifactCommentsCmd = &cobra.Command{
	Use:   "comments <ref>",
	Short: "List a review's comments, suggestions and highlights",
	Long: `List the comments, suggested changes and highlights of a review as
structured entries, without parsing the CriticMarkup text.

<ref> names an artifact or one of its versions. A bare reference
(scion://artifact/<id>, or <id>) lists the newest review of the artifact;
scion://artifact/<id>@<seq> lists that version, which must be a review.

Each entry shows its kind (comment, suggestion or highlight), the file,
line and column it starts at, the text it is about, and the comment or
the suggested text. A comment written right after a highlight or a
suggestion belongs to it. Every entry of a review has the review's author
and time. The review's note is shown first.

With --format json the output is one JSON object (schemaVersion 1).

Examples:
  scion artifact comments scion://artifact/5f1c2d3e-...
  scion artifact comments scion://artifact/5f1c2d3e-...@3 --format json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, _, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
		defer cancel()
		return listReviewComments(ctx, client.Artifacts(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], isJSONOutput())
	},
}

func init() {
	artifactCmd.AddCommand(artifactCommentsCmd)
}

// reviewCommentsSchemaVersion is the schemaVersion of the JSON output. It
// changes only when a field is removed or changes meaning; new fields may
// be added within a version.
const reviewCommentsSchemaVersion = 1

// reviewComments is the JSON output of artifact comments.
type reviewComments struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Artifact      string               `json:"artifact"`
	Ref           string               `json:"ref"`
	Seq           int                  `json:"seq"`
	Author        reviewAuthor         `json:"author"`
	CreatedAt     time.Time            `json:"createdAt"`
	Note          string               `json:"note"`
	Entries       []reviewCommentEntry `json:"entries"`
}

// reviewAuthor is who published a review version.
type reviewAuthor struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// reviewCommentEntry is one entry; see critic.Entry for the fields.
type reviewCommentEntry struct {
	Kind       critic.EntryKind `json:"kind"`
	Change     critic.Change    `json:"change,omitempty"`
	File       string           `json:"file"`
	Line       int              `json:"line"`
	Column     int              `json:"column"`
	Start      int              `json:"start"`
	End        int              `json:"end"`
	Anchor     string           `json:"anchor"`
	Comment    string           `json:"comment"`
	Suggestion string           `json:"suggestion"`
	Author     reviewAuthor     `json:"author"`
	CreatedAt  time.Time        `json:"createdAt"`
}

// listReviewComments prints the entries of the review that ref names: the
// version it pins, which must be a review, or else the newest review.
// Every read goes through the hub's read routes, so the access rules are
// those of artifact get.
func listReviewComments(ctx context.Context, svc hubclient.ArtifactService, stdout, stderr io.Writer, ref string, asJSON bool) error {
	rc, err := fetchReviewComments(ctx, svc, stderr, ref)
	if err != nil {
		return err
	}
	if asJSON {
		return encodeReviewCommentsJSON(stdout, rc)
	}
	return printReviewComments(stdout, rc)
}

// encodeReviewCommentsJSON writes the JSON output.
func encodeReviewCommentsJSON(out io.Writer, rc *reviewComments) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rc)
}

// fetchReviewComments resolves ref to a review version, reads its text
// files (each verified against its recorded digest) and lists their marks.
func fetchReviewComments(ctx context.Context, svc hubclient.ArtifactService, stderr io.Writer, ref string) (*reviewComments, error) {
	id, seq, err := artifacts.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	var meta *hubclient.ArtifactResponse
	if seq > 0 {
		if meta, err = artifactVersionFor(ctx, svc, id, seq, ""); err != nil {
			return nil, err
		}
		if meta.Version.Kind != artifacts.VersionKindReview {
			return nil, fmt.Errorf("%s is a %s version, not a review; list the versions with scion artifact versions %s",
				artifacts.FormatRef(id, seq), meta.Version.Kind, artifacts.FormatRef(id, 0))
		}
	} else {
		if meta, err = artifactVersionFor(ctx, svc, id, 0, artifacts.VersionKindReview); err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(stderr, "Using %s (review)\n", artifacts.FormatRef(id, meta.Version.Seq))
	}
	v := meta.Version
	author := reviewAuthor{Kind: v.CreatedByKind, Ref: v.CreatedByRef}
	rc := &reviewComments{
		SchemaVersion: reviewCommentsSchemaVersion,
		Artifact:      artifacts.FormatRef(id, 0),
		Ref:           artifacts.FormatRef(id, v.Seq),
		Seq:           v.Seq,
		Author:        author,
		CreatedAt:     v.CreatedAt.UTC(),
		Note:          v.Note,
		Entries:       []reviewCommentEntry{},
	}
	for _, f := range reviewTextFiles(v) {
		text, err := readVerifiedFile(ctx, svc, id, v.Seq, f)
		if err != nil {
			return nil, err
		}
		for _, e := range critic.Entries(text) {
			rc.Entries = append(rc.Entries, reviewCommentEntry{
				Kind: e.Kind, Change: e.Change, File: f.Path,
				Line: e.Line, Column: e.Column, Start: e.Start, End: e.End,
				Anchor: e.Anchor, Comment: e.Comment, Suggestion: e.Suggestion,
				Author: author, CreatedAt: rc.CreatedAt,
			})
		}
	}
	return rc, nil
}

// reviewTextFiles is the text files of a version, entry file first and
// then the others in the order the hub lists them.
func reviewTextFiles(v *hubclient.ArtifactVersion) []hubclient.ArtifactFile {
	var entry, rest []hubclient.ArtifactFile
	for _, f := range bundleFiles(v.Files) {
		switch {
		case !artifacts.IsTextMediaType(f.MediaType):
		case f.Path == v.EntryPath:
			entry = append(entry, f)
		default:
			rest = append(rest, f)
		}
	}
	return append(entry, rest...)
}

// readVerifiedFile reads file f of version seq into memory (at most f.Size
// bytes), checked against its recorded size and digest.
func readVerifiedFile(ctx context.Context, svc hubclient.ArtifactService, id string, seq int, f hubclient.ArtifactFile) ([]byte, error) {
	body, err := svc.OpenFile(ctx, id, seq, f.Path)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w%s", f.Path, err, artifactErrorHint(err, false))
	}
	defer func() { _ = body.Close() }()
	var buf bytes.Buffer
	if err := copyVerified(&buf, body, f.SHA256, f.Size); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	return buf.Bytes(), nil
}

// printReviewComments writes the human-readable listing.
func printReviewComments(out io.Writer, rc *reviewComments) error {
	var b strings.Builder
	by := rc.Author.Kind
	if rc.Author.Ref != "" {
		by += ":" + rc.Author.Ref
	}
	fmt.Fprintf(&b, "%s  review by %s, %s\n", safeText(rc.Ref), safeText(by), clitime.Format(rc.CreatedAt, clitime.Minute))
	if rc.Note != "" {
		fmt.Fprintf(&b, "Note: %s\n", commentText(rc.Note, "      "))
	}
	counts := map[critic.EntryKind]int{}
	for _, e := range rc.Entries {
		counts[e.Kind]++
		what := string(e.Kind)
		if e.Change != "" {
			what += " (" + string(e.Change) + ")"
		}
		fmt.Fprintf(&b, "\n%s:%d:%d  %s\n", safeText(e.File), e.Line, e.Column, what)
		switch {
		case e.Change == critic.ChangeReplace:
			fmt.Fprintf(&b, "  %s -> %s\n", quoteText(e.Anchor), quoteText(e.Suggestion))
		case e.Change == critic.ChangeInsert:
			fmt.Fprintf(&b, "  insert %s\n", quoteText(e.Suggestion))
		case e.Change == critic.ChangeDelete:
			fmt.Fprintf(&b, "  delete %s\n", quoteText(e.Anchor))
		case e.Anchor != "":
			fmt.Fprintf(&b, "  on %s\n", quoteText(e.Anchor))
		}
		if e.Comment != "" {
			fmt.Fprintf(&b, "  %s\n", commentText(e.Comment, "  "))
		}
	}
	if len(rc.Entries) == 0 {
		b.WriteString("\nNo comments, suggestions or highlights.\n")
	} else {
		fmt.Fprintf(&b, "\n%s, %s, %s\n", plural(counts[critic.EntryComment], "comment"),
			plural(counts[critic.EntrySuggestion], "suggestion"), plural(counts[critic.EntryHighlight], "highlight"))
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// quoteText quotes text from the review for one line of output.
func quoteText(s string) string {
	return fmt.Sprintf("%q", s)
}

// commentText is a comment made safe for the terminal, with each line
// after the first indented by indent.
func commentText(s, indent string) string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n"), "\n")
	for i, l := range lines {
		lines[i] = safeText(l)
	}
	return strings.Join(lines, "\n"+indent)
}

// plural is "1 comment" or "2 comments".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"golang.org/x/net/html"
)

var (
	artifactViewCheck    bool
	artifactViewShowLink bool
)

var artifactViewCmd = &cobra.Command{
	Use:   "view <ref>",
	Short: "Print a view link for an HTML artifact and check its links",
	Long: `Print a short-lived link that shows an artifact version whose entry is
HTML, the way the web UI shows it. The link opens the entry page and
serves the version's other files to it. It expires after a short time
(30 minutes unless the hub says otherwise), and the command never opens
a browser.

<ref> is scion://artifact/<id> (the current version) or
scion://artifact/<id>@<seq>.

With --check, the command also reads the version's HTML files and reports
every href or src that does not name a file of that version, and exits
with an error if it finds one. A link that starts with "/" or climbs above
the bundle's top folder is reported too: it leaves the view. Links with a
scheme (https:, mailto:, data:, ...), links to another host, and links to
a fragment of the same page are not checked, and a link that cannot be
parsed is reported. A <base href> in a page is not taken into account:
links are resolved against the page's own folder.

In agent mode the command prints the version and the link's expiry but
not the link itself; --show-link prints the link too. In human mode the
link is always printed.

Examples:
  scion artifact view scion://artifact/5f1c2d3e-...
  scion artifact view scion://artifact/5f1c2d3e-...@3 --check
  scion artifact view scion://artifact/5f1c2d3e-... --show-link`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		hubEndpoint, svc, err := artifactViewClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		return viewArtifact(ctx, svc, cmd.OutOrStdout(), hubEndpoint, args[0],
			viewOptions{
				Check:    artifactViewCheck,
				JSON:     isJSONOutput(),
				HideLink: viewHidesLink(resolveMode(), artifactViewShowLink),
			})
	},
}

// artifactViewClient returns the hub endpoint and the artifact service
// artifact view uses. Tests replace it to run the command against a test
// hub.
var artifactViewClient = func() (string, hubclient.ArtifactService, error) {
	settings, client, err := requireArtifactHubClient()
	if err != nil {
		return "", nil, err
	}
	return GetHubEndpoint(settings), client.Artifacts(), nil
}

func init() {
	artifactViewCmd.Flags().BoolVar(&artifactViewCheck, "check", false, "Also report href and src links that name no file of the version")
	artifactViewCmd.Flags().BoolVar(&artifactViewShowLink, "show-link", false, "Print the view link in agent mode too (human mode always prints it)")
	artifactCmd.AddCommand(artifactViewCmd)
}

// viewOptions are the flags of artifact view.
type viewOptions struct {
	Check bool
	JSON  bool
	// HideLink leaves the view link out of the output, text and JSON
	// alike; the version and the link's expiry are still printed.
	HideLink bool
}

// viewHidesLink reports whether artifact view leaves the link out: in
// agent mode unless --show-link is given, and never in human mode.
func viewHidesLink(mode CLIMode, showLink bool) bool {
	return mode == ModeAgent && !showLink
}

// brokenLink is a relative link of an HTML file that names no file of the
// version.
type brokenLink struct {
	File      string `json:"file"`
	Attribute string `json:"attribute"`
	Target    string `json:"target"`
}

// viewResult is the JSON output of artifact view.
type viewResult struct {
	Ref          string       `json:"ref"`
	URL          string       `json:"url,omitempty"`
	LinkHidden   bool         `json:"linkHidden,omitempty"`
	ExpiresAt    time.Time    `json:"expiresAt"`
	RemoteImages bool         `json:"remoteImages,omitempty"`
	Checked      bool         `json:"checked"`
	HTMLFiles    int          `json:"htmlFiles"`
	Broken       []brokenLink `json:"broken"`
}

// errBrokenLinks is returned by viewArtifact when --check found links
// that name no file of the version.
var errBrokenLinks = errors.New("broken links found")

// viewArtifact mints a view link for the version ref names and prints it.
// With opts.Check it also reports the relative links of the version's HTML
// files that name no file of the version, returning errBrokenLinks if any.
func viewArtifact(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, hubEndpoint, ref string, opts viewOptions) error {
	id, seq, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	meta, err := artifactVersionFor(ctx, svc, id, seq, "")
	if err != nil {
		return err
	}
	v := meta.Version
	view, err := svc.MintView(ctx, id, v.Seq)
	if err != nil {
		return fmt.Errorf("view: %w%s", err, viewErrorHint(err))
	}
	res := viewResult{
		Ref:          artifacts.FormatRef(id, v.Seq),
		URL:          absoluteHubURL(hubEndpoint, view.URL),
		ExpiresAt:    view.ExpiresAt,
		RemoteImages: view.RemoteImages,
		Checked:      opts.Check,
		Broken:       []brokenLink{},
	}
	if opts.HideLink {
		res.URL = ""
		res.LinkHidden = true
	}
	if opts.Check {
		files, broken, err := checkBundleLinks(ctx, svc, id, v)
		if err != nil {
			return err
		}
		res.HTMLFiles = files
		res.Broken = broken
	}
	if opts.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		printViewResult(out, res)
	}
	if len(res.Broken) > 0 {
		return fmt.Errorf("%w: %d in %s", errBrokenLinks, len(res.Broken), res.Ref)
	}
	return nil
}

func printViewResult(out io.Writer, res viewResult) {
	if !res.LinkHidden {
		_, _ = fmt.Fprintf(out, "%s\n", safeText(res.URL))
	}
	_, _ = fmt.Fprintf(out, "View of %s, valid until %s\n", res.Ref, clitime.Format(res.ExpiresAt, clitime.Minute))
	if res.LinkHidden {
		_, _ = fmt.Fprintln(out, "The link is not printed in agent mode; run with --show-link to print it.")
	}
	if res.RemoteImages {
		_, _ = fmt.Fprintln(out, "Note: the entry references remote images, which the view does not load.")
	}
	if !res.Checked {
		return
	}
	if len(res.Broken) == 0 {
		_, _ = fmt.Fprintf(out, "Checked %d HTML file(s): every relative link names a file of the version.\n", res.HTMLFiles)
		return
	}
	_, _ = fmt.Fprintf(out, "Checked %d HTML file(s): %d relative link(s) name no file of the version:\n", res.HTMLFiles, len(res.Broken))
	for _, b := range res.Broken {
		_, _ = fmt.Fprintf(out, "  %s: %s=%q\n", safeText(b.File), b.Attribute, b.Target)
	}
}

// absoluteHubURL joins the hub endpoint and a hub path. Without an
// endpoint the path is returned as is.
func absoluteHubURL(hubEndpoint, p string) string {
	if hubEndpoint == "" {
		return p
	}
	return strings.TrimRight(hubEndpoint, "/") + "/" + strings.TrimLeft(p, "/")
}

// viewErrorHint explains the view route's answers on top of
// artifactErrorHint.
func viewErrorHint(err error) string {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusBadRequest:
			return "\nOnly a version whose entry file is HTML has a view."
		case http.StatusServiceUnavailable:
			return "\nThis hub has no artifact views configured."
		}
	}
	return artifactErrorHint(err, false)
}

// checkBundleLinks reads the HTML files of version v and returns how many
// it read and the relative href and src links that name no file of v.
func checkBundleLinks(ctx context.Context, svc hubclient.ArtifactService, id string, v *hubclient.ArtifactVersion) (int, []brokenLink, error) {
	present := make(map[string]bool, len(v.Files))
	for _, f := range v.Files {
		present[f.Path] = true
	}
	var htmlFiles []hubclient.ArtifactFile
	for _, f := range v.Files {
		if isHTMLMediaType(f.MediaType) {
			htmlFiles = append(htmlFiles, f)
		}
	}
	sort.Slice(htmlFiles, func(i, j int) bool { return htmlFiles[i].Path < htmlFiles[j].Path })
	broken := []brokenLink{}
	for _, f := range htmlFiles {
		var buf bytes.Buffer
		rc, err := svc.OpenFile(ctx, id, v.Seq, f.Path)
		if err != nil {
			return 0, nil, fmt.Errorf("read %s: %w%s", safeText(f.Path), err, artifactErrorHint(err, false))
		}
		err = copyVerified(&buf, rc, f.SHA256, f.Size)
		_ = rc.Close()
		if err != nil {
			return 0, nil, fmt.Errorf("read %s: %w", safeText(f.Path), err)
		}
		for _, l := range relativeLinks(buf.Bytes()) {
			if target, ok := resolveBundleLink(f.Path, l.target); ok && !linkPresent(present, target) {
				broken = append(broken, brokenLink{File: f.Path, Attribute: l.attr, Target: l.target})
			}
		}
	}
	return len(htmlFiles), broken, nil
}

// isHTMLMediaType reports whether a file's media type is HTML.
func isHTMLMediaType(mt string) bool {
	mt, _, _ = strings.Cut(mt, ";")
	return strings.EqualFold(strings.TrimSpace(mt), "text/html")
}

// htmlLink is an href or src attribute value of an HTML document.
type htmlLink struct {
	attr   string
	target string
}

// relativeLinks returns the href and src attribute values of doc, in
// document order. Values are returned as written; resolveBundleLink
// decides which are relative.
func relativeLinks(doc []byte) []htmlLink {
	var out []htmlLink
	z := html.NewTokenizer(bytes.NewReader(doc))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			for {
				key, val, more := z.TagAttr()
				if k := string(key); k == "href" || k == "src" {
					if v := strings.TrimSpace(string(val)); v != "" {
						out = append(out, htmlLink{attr: k, target: v})
					}
				}
				if !more {
					break
				}
			}
		}
	}
}

// resolveBundleLink resolves link, found in the file at from, to the path
// within the version it names. ok is false for links that are not
// checked: links with a scheme, links to another host, and fragment-only
// or query-only links. A link that cannot be parsed is returned as written,
// and is reported. A link that starts with "/" or climbs above the
// bundle's top folder leaves the view, so it is returned as a path no file
// has ("/..." or "../..."), and is reported.
func resolveBundleLink(from, link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil {
		// Reported as written: a browser cannot follow it either.
		return link, true
	}
	if u.Scheme != "" || u.Host != "" || u.Opaque != "" || strings.HasPrefix(link, "//") {
		return "", false
	}
	p := u.Path
	if p == "" {
		return "", false
	}
	if strings.HasPrefix(p, "/") {
		return p, true
	}
	rel := path.Clean(path.Join(path.Dir(from), p))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return rel, true
	}
	if rel == "." {
		return "index.html", true
	}
	if strings.HasSuffix(p, "/") {
		return rel + "/index.html", true
	}
	return rel, true
}

// linkPresent reports whether target is a file of the version, or a
// folder of it that holds an index.html.
func linkPresent(present map[string]bool, target string) bool {
	return present[target] || present[target+"/index.html"]
}

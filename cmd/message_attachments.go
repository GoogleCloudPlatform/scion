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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

// Attachments stored as artifacts (ptone/scion#4596, section 7.1). When the
// hub's probe answers "artifact", --attach uploads the files and the send
// names the uploads by id; otherwise today's validation and staging run
// unchanged.

// msgAttachUpload is set for a send whose --attach files are uploaded as
// artifacts; nil means today's staged-path attachments.
var msgAttachUpload *attachmentUpload

// attachmentUpload uploads the --attach files of one send, once per
// recipient send.
type attachmentUpload struct {
	uploader hubclient.AttachmentUploader
	paths    []string
}

// htmlRefusal is the error for an HTML or SVG attachment.
const htmlRefusal = "attachments cannot be HTML or SVG; publish it as an artifact instead: scion artifact publish <file>, then send with --artifact <ref>"

// markdownHintBytes is the size above which a Markdown attachment gets a
// hint to publish it as an artifact instead.
const markdownHintBytes = 64 * 1024

// probeAttachmentUploads asks the hub where this credential's uploads go.
// It returns a nil uploader (use today's path) when the hub answers
// anything but "artifact", or refuses the probe with 403, 404 or 405 (a hub
// from before the probe), or the client cannot upload; fallback then names
// the reason. Any other error is returned.
func probeAttachmentUploads(ctx context.Context, client hubclient.Client) (uploader hubclient.AttachmentUploader, probe *hubclient.AttachmentProbe, fallback string, err error) {
	uploader, ok := client.(hubclient.AttachmentUploader)
	if !ok {
		return nil, nil, "this client cannot upload attachments", nil
	}
	probe, err = uploader.ProbeAttachmentUploads(ctx)
	if err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
				return nil, nil, fmt.Sprintf("the hub refused the upload probe with status %d", apiErr.StatusCode), nil
			}
		}
		return nil, nil, "", fmt.Errorf("attachments: could not ask the hub where uploads go: %w", err)
	}
	if probe == nil || probe.Uploads == "" {
		return nil, nil, "the hub did not offer uploads", nil
	}
	if probe.Uploads != hubclient.AttachmentUploadsArtifact {
		return nil, nil, fmt.Sprintf("the hub answered %q", probe.Uploads), nil
	}
	return uploader, probe, "", nil
}

// prepareAttachmentUpload checks the --attach files for an upload and
// returns the upload, or nil when the hub takes today's staged paths. With
// toAgent, the total size must fit the hub's delivery budget; fallback
// names why the files are staged instead. Nothing is uploaded here.
func prepareAttachmentUpload(client hubclient.Client, paths []string, toAgent bool) (*attachmentUpload, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	uploader, probe, fallback, err := probeAttachmentUploads(ctx, client)
	if err != nil || uploader == nil {
		return nil, fallback, err
	}
	resolved, err := checkAttachmentFiles(paths, toAgent, probe.DeliveryMaxBytes)
	if err != nil {
		return nil, "", err
	}
	return &attachmentUpload{uploader: uploader, paths: resolved}, "", nil
}

// checkAttachmentFiles applies the upload guard rails and returns the
// absolute paths: each file exists, is a regular file and is readable; no
// two share a base name (case-insensitive); no file is HTML or SVG; with
// toAgent and a budget, the total fits it. A large Markdown file gets a
// hint on stderr.
func checkAttachmentFiles(paths []string, toAgent bool, budget *int64) ([]string, error) {
	resolved := make([]string, 0, len(paths))
	seen := make(map[string]string, len(paths))
	reduced := make(map[string]string, len(paths))
	var total int64
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", p, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			if info.IsDir() {
				return nil, fmt.Errorf("attachment %q: is a directory, not a regular file", p)
			}
			return nil, fmt.Errorf("attachment %q: is not a regular file", p)
		}
		head, err := readAttachmentHead(abs)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", p, err)
		}
		if isHTMLOrSVG(abs, head) {
			return nil, newUsageError("attachment %q: %s", p, htmlRefusal)
		}
		base := strings.ToLower(filepath.Base(abs))
		if other, dup := seen[base]; dup {
			return nil, newUsageError("attachments %q and %q have the same file name; rename one", other, p)
		}
		seen[base] = p
		// The hub delivers each file under its reduced name and refuses two that
		// reduce to one name; refusing here keeps the upload from happening.
		r := strings.ToLower(messaging.ReduceAttachmentName(filepath.Base(abs)))
		if other, dup := reduced[r]; dup {
			return nil, newUsageError("attachments %q and %q would be delivered under one file name; rename one", other, p)
		}
		reduced[r] = p
		total += info.Size()
		if isMarkdown(abs) && info.Size() > markdownHintBytes {
			fmt.Fprintf(os.Stderr, "Hint: large Markdown is easier to review as an artifact: scion artifact publish %s\n", p)
		}
		resolved = append(resolved, abs)
	}
	if toAgent && budget != nil {
		if *budget == 0 {
			return nil, newUsageError("attachments cannot be delivered to agents on this hub: attachment delivery is off")
		}
		if total > *budget {
			return nil, newUsageError("attachments total %d bytes, over the hub's limit of %d bytes for a message to an agent", total, *budget)
		}
	}
	return resolved, nil
}

// readAttachmentHead reads up to the first 512 bytes of the file at path,
// which also checks that it is readable.
func readAttachmentHead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 512)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// isHTMLOrSVG reports whether the file is HTML or SVG, by extension or by
// its first bytes.
func isHTMLOrSVG(path string, head []byte) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm", ".svg":
		return true
	}
	lower := bytes.ToLower(head)
	return bytes.Contains(lower, []byte("<html")) || bytes.Contains(lower, []byte("<svg"))
}

func isMarkdown(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

// ids uploads the files and returns their ids. If any file is not stored,
// nothing is sent: the per-file failures are returned as one error.
func (u *attachmentUpload) ids(parent context.Context) ([]string, error) {
	// An upload of up to the delivery budget can outlast a send's timeout.
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	result, err := u.uploader.UploadAttachments(ctx, u.paths)
	if err != nil {
		return nil, fmt.Errorf("attachment upload failed: %w", err)
	}
	if len(result.Failures) > 0 || len(result.Attachments) != len(u.paths) {
		var lines []string
		for _, f := range result.Failures {
			lines = append(lines, fmt.Sprintf("  %s: %s", f.Name, f.Error))
		}
		if len(lines) == 0 {
			lines = append(lines, fmt.Sprintf("  the hub stored %d of %d files", len(result.Attachments), len(u.paths)))
		}
		return nil, fmt.Errorf("attachment upload failed; nothing was sent:\n%s", strings.Join(lines, "\n"))
	}
	ids := make([]string, 0, len(result.Attachments))
	for _, a := range result.Attachments {
		ids = append(ids, a.ID)
	}
	return ids, nil
}

// attachmentIDsForSend uploads the --attach files for one recipient send
// when they go to the hub as artifacts, and returns their ids (nil
// otherwise).
func attachmentIDsForSend(ctx context.Context) ([]string, error) {
	if msgAttachUpload == nil {
		return nil, nil
	}
	return msgAttachUpload.ids(ctx)
}

// resolveAttachMode decides how the --attach files of a send travel: it
// asks the hub's probe first (prepareAttachmentUpload); with "artifact" it
// returns the upload and no staged paths, otherwise it runs today's path
// validation and returns the paths to stage, unchanged, and the reason they
// are staged (for stageAttachmentsWithWarning).
func resolveAttachMode(client hubclient.Client, paths []string, toAgent bool) (*attachmentUpload, []string, string, error) {
	upload, fallback, err := prepareAttachmentUpload(client, paths, toAgent)
	if err != nil {
		return nil, nil, "", err
	}
	if upload != nil {
		return upload, nil, "", nil
	}
	if err := validateStagedAttachmentPaths(paths); err != nil {
		return nil, nil, "", err
	}
	return nil, paths, fallback, nil
}

// stageAttachmentsWithWarning stages the --attach files (stageAttachments)
// and, once they are staged, prints one stderr line saying why they were
// staged instead of uploaded (ptone/scion#6549). Nothing is printed for no
// files or when staging fails.
func stageAttachmentsWithWarning(paths []string, fallback string) ([]string, error) {
	if len(paths) == 0 {
		return paths, nil
	}
	staged, err := stageAttachments(paths)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "Warning: attachments are staged on the local scratchpad instead of uploaded to the hub (%s); they may not reach the hub or recipients on another broker\n", fallback)
	return staged, nil
}

// validateStagedAttachmentPaths is today's validation of --attach paths for
// staging: each must be under an allowed root and be a regular file.
func validateStagedAttachmentPaths(paths []string) error {
	for _, p := range paths {
		resolved := resolveAttachmentPath(p)
		if resolved == "" {
			return fmt.Errorf("attachment %q: path is outside allowed roots (/workspace, /scion-volumes)", p)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("attachment %q: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			if info.IsDir() {
				return fmt.Errorf("attachment %q: is a directory, not a regular file", p)
			}
			return fmt.Errorf("attachment %q: is not a regular file", p)
		}
	}
	return nil
}

// groupHasAgent reports whether a group send has an agent recipient.
func groupHasAgent(recipients []messages.GroupRecipient) bool {
	for _, r := range recipients {
		if r.Kind == messages.RecipientAgent {
			return true
		}
	}
	return false
}

// msgSendTimeout bounds one send request. Tests shorten it.
var msgSendTimeout = 30 * time.Second

// uploadThenSendContext uploads the --attach files for one recipient send
// when they go to the hub as artifacts (attachmentIDsForSend, with its own
// bound), and only then starts the send's own timeout, so a slow upload
// never uses up the time of the send that names it. parent is the context
// the send belongs to (a group send's fan-out context, or the background).
// On error the returned cancel is a no-op.
func uploadThenSendContext(parent context.Context) ([]string, context.Context, context.CancelFunc, error) {
	ids, err := attachmentIDsForSend(parent)
	if err != nil {
		return nil, parent, func() {}, err
	}
	ctx, cancel := context.WithTimeout(parent, msgSendTimeout)
	return ids, ctx, cancel, nil
}

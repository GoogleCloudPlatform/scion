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
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"golang.org/x/text/unicode/norm"
)

// messageAttachmentJSON is the CLI-only JSON view of a MessageAttachment when
// --save-attachments is combined with --json. SavedPath and Skipped are
// CLI-side annotations and are never sent by the hub.
type messageAttachmentJSON struct {
	store.MessageAttachment
	SavedPath string `json:"savedPath,omitempty"`
	Skipped   string `json:"skipped,omitempty"`
}

// messageWithSaveJSON wraps store.Message for --json + --save-attachments
// output so Attachments carries CLI-side savedPath/skipped annotations.
type messageWithSaveJSON struct {
	*store.Message
	Attachments []messageAttachmentJSON `json:"attachments,omitempty"`
}

// formatMessageAttachmentLine formats one attachment line for get-message text
// output (design §3.4).
func formatMessageAttachmentLine(a store.MessageAttachment) string {
	ref := a.Ref
	if ref == "" {
		ref = artifacts.FormatAttachmentRef(a.ID)
	}
	return fmt.Sprintf("Attachment: %s (%s, %s) %s",
		a.Name, messaging.FormatAttachmentSize(a.Size), a.ContentType, ref)
}

// writeMessageAttachmentsText writes the per-attachment lines after a message
// body in text mode.
func writeMessageAttachmentsText(w io.Writer, atts []store.MessageAttachment) error {
	for _, a := range atts {
		if _, err := fmt.Fprintln(w, formatMessageAttachmentLine(a)); err != nil {
			return err
		}
		for _, pub := range a.PublishedAs {
			if _, err := fmt.Fprintf(w, "  published as %s\n", pub); err != nil {
				return err
			}
		}
	}
	return nil
}

// windowsReservedNames are the case-insensitive device names that Windows
// rejects as file stems (with or without an extension).
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// safeAttachmentFilename reduces an uploaded attachment name to a safe local
// filename that passes checkBundleName on both Unix and Windows.
func safeAttachmentFilename(name string) (string, error) {
	reduced := messaging.ReduceAttachmentName(name)
	stem := reduced
	if dot := strings.IndexByte(reduced, '.'); dot >= 0 {
		stem = reduced[:dot]
	}
	if windowsReservedNames[strings.ToUpper(stem)] {
		reduced = "attachment-" + reduced
	}
	if err := checkBundleName(reduced); err != nil {
		return "", err
	}
	return reduced, nil
}

// insertIDBeforeExt inserts "-<shortID>" before the final extension of name
// (if any).
func insertIDBeforeExt(name, id string) string {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	short = messaging.ReduceAttachmentName(short)
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		return name[:dot] + "-" + short + name[dot:]
	}
	return name + "-" + short
}

// planAttachmentFilenames computes safe, collision-free filenames for atts
// within a single save run. Collisions after case-folding and Unicode NFC
// normalization get "-<first 8 of id>" inserted before the extension.
func planAttachmentFilenames(atts []store.MessageAttachment) ([]string, error) {
	names := make([]string, len(atts))
	seen := make(map[string]struct{}, len(atts))
	for i, a := range atts {
		name, err := safeAttachmentFilename(a.Name)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", a.Name, err)
		}
		key := strings.ToLower(norm.NFC.String(name))
		if _, dup := seen[key]; dup {
			name = insertIDBeforeExt(name, a.ID)
			if err := checkBundleName(name); err != nil {
				return nil, fmt.Errorf("attachment %q: %w", a.Name, err)
			}
			key = strings.ToLower(norm.NFC.String(name))
			if _, dup2 := seen[key]; dup2 {
				name = insertIDBeforeExt(name, a.ID+"-"+fmt.Sprintf("%d", i))
				if err := checkBundleName(name); err != nil {
					return nil, fmt.Errorf("attachment %q: %w", a.Name, err)
				}
				key = strings.ToLower(norm.NFC.String(name))
			}
		}
		seen[key] = struct{}{}
		names[i] = name
	}
	return names, nil
}

// selectMessageAttachments filters msg.Attachments by exact ID or exact Name
// when filters is non-empty. Returns an error if any filter matches nothing.
func selectMessageAttachments(atts []store.MessageAttachment, filters []string) ([]store.MessageAttachment, error) {
	if len(filters) == 0 {
		return atts, nil
	}
	matched := make(map[string]bool, len(filters))
	selected := make([]store.MessageAttachment, 0, len(filters))
	added := make(map[int]bool, len(filters))
	for i, a := range atts {
		for _, f := range filters {
			if a.ID == f || a.Name == f {
				matched[f] = true
				if !added[i] {
					added[i] = true
					selected = append(selected, a)
				}
			}
		}
	}
	for _, f := range filters {
		if !matched[f] {
			return nil, fmt.Errorf("attachment %q not found on message", f)
		}
	}
	return selected, nil
}

func isAlreadyExistsWriteErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrExist) || strings.Contains(err.Error(), "already exists")
}

// saveMessageAttachments downloads and verifies each selected attachment of
// msg into dir using outDir. It returns per-attachment CLI annotations (aligned
// with msg.Attachments) and any error encountered.
func saveMessageAttachments(
	ctx context.Context,
	svc hubclient.ConversationService,
	stderr io.Writer,
	conversationID string,
	msg *store.Message,
	dir string,
	filters []string,
	force bool,
	includePublished bool,
) ([]messageAttachmentJSON, error) {
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		return nil, fmt.Errorf("%s exists and is not a directory", dir)
	}

	selected, err := selectMessageAttachments(msg.Attachments, filters)
	if err != nil {
		return nil, err
	}

	annotations := make([]messageAttachmentJSON, len(msg.Attachments))
	indexByID := make(map[string]int, len(msg.Attachments))
	for i, a := range msg.Attachments {
		annotations[i] = messageAttachmentJSON{MessageAttachment: a}
		indexByID[a.ID] = i
	}

	if len(selected) == 0 {
		return annotations, nil
	}

	toDownload := make([]store.MessageAttachment, 0, len(selected))
	for _, a := range selected {
		if !includePublished && len(a.PublishedAs) > 0 {
			if idx, ok := indexByID[a.ID]; ok {
				annotations[idx].Skipped = "published"
			}
			_, _ = fmt.Fprintf(stderr, "skipped %s: published as %s (use 'scion artifact get', or --include-published)\n", a.Name, a.PublishedAs[0])
			continue
		}
		toDownload = append(toDownload, a)
	}

	if len(toDownload) == 0 {
		return annotations, nil
	}

	names, err := planAttachmentFilenames(toDownload)
	if err != nil {
		return nil, err
	}

	out, err := openOutDir(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Close() }()

	var existsErr error
	for i, a := range toDownload {
		filename := names[i]
		rc, derr := svc.DownloadAttachment(ctx, conversationID, msg.ID, a.ID)
		if derr != nil {
			var apiErr *apiclient.APIError
			if errors.As(derr, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				_, _ = fmt.Fprintf(stderr, "attachment %s no longer available\n", a.Name)
				continue
			}
			return annotations, fmt.Errorf("download %s: %w", a.Name, derr)
		}
		werr := out.WriteFile(filename, force, func(w io.Writer) error {
			return copyVerified(w, rc, a.SHA256, a.Size)
		})
		_ = rc.Close()
		if werr != nil {
			if isAlreadyExistsWriteErr(werr) {
				if idx, ok := indexByID[a.ID]; ok {
					annotations[idx].Skipped = "exists"
				}
				if existsErr == nil {
					existsErr = werr
				}
				continue
			}
			return annotations, fmt.Errorf("save %s: %w", a.Name, werr)
		}
		savedPath := filepath.Join(dir, filename)
		if idx, ok := indexByID[a.ID]; ok {
			annotations[idx].SavedPath = savedPath
		}
	}

	return annotations, existsErr
}

func outputMessageWithSaveJSON(w io.Writer, msg *store.Message, annotations []messageAttachmentJSON) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(messageWithSaveJSON{
		Message:     msg,
		Attachments: annotations,
	})
}

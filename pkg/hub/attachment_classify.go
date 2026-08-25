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

package hub

import (
	"fmt"
	"net/http"
	"strings"
)

// Attachment classification.
//
// The MIME type an upload is stored under is derived from the bytes plus the
// filename, never from the type the client declares. A browser sends
// application/octet-stream for anything it does not recognise — which is every
// developer format worth attaching, config files and logs and source — so
// trusting the declared type rejected exactly the files people wanted to send
// (#1045). Trusting it in the other direction is worse: a declared type is a
// claim the uploader controls, and an allowlist checked against a claim is not
// an allowlist.

// textLikeExtensions are the extensions accepted as plain text on the strength
// of the extension, once the content has been confirmed to be text. They are
// the developer formats a chat about software carries: config, data, logs,
// source. Nothing here is executed or rendered by the browser — downloads go
// out with nosniff and, for non-images, an attachment disposition.
//
// Deliberately absent: .js and every other entry of DangerousExtensions, and
// the markup extensions of refusedMarkupExtensions. .jsx is here and .js is
// not, because the blocklist is about what a browser will run when a file
// escapes the download path, and .jsx is not that file.
var textLikeExtensions = map[string]bool{
	".json": true, ".yaml": true, ".yml": true, ".toml": true,
	".ini": true, ".cfg": true, ".env": true, ".log": true,
	".csv": true, ".xml": true, ".diff": true, ".patch": true,
	".txt": true, ".md": true, ".rst": true, ".adoc": true,
	".sql": true, ".graphql": true, ".proto": true,
	".ts": true, ".tsx": true, ".jsx": true, ".py": true,
	".go": true, ".rs": true, ".rb": true, ".java": true,
	".kt": true, ".swift": true, ".c": true, ".cpp": true,
	".h": true, ".hpp": true, ".cs": true,
}

// refusedMarkupExtensions are refused on the extension alone, whatever their
// bytes sniff as.
//
// The allowlist keeps out the sniffed type text/html. It does not keep out
// .html files: http.DetectContentType answers text/html for seventeen fixed tag
// signatures, and <img>, <svg> and <video> are not among them, so a document
// built from those sniffs as text/plain. Before this check, an .html file
// containing <img src=x onerror=…> was accepted and stored as text — inert,
// because the download path sends nosniff and an attachment disposition and the
// preview goes to a text sink, but inert only for as long as all three of those
// hold at once.
//
// None of these is in textLikeExtensions, so refusing them removes nothing the
// change set out to accept; and main already refused them in practice, since a
// browser declares text/html or image/svg+xml for these files and the old gate
// checked that declaration against the same allowlist. This restores that
// parity rather than tightening past it.
//
// .hta is not here: it is in DangerousExtensions, with the executables it
// belongs to.
//
// Enforced in SanitizeFilename as well as here, so the agent --attach path
// refuses these too — it never calls ClassifyAttachment. The check stays in
// both places because ClassifyAttachment is also usable on its own, and the
// two maps should not disagree about which files exist.
var refusedMarkupExtensions = map[string]bool{
	".html": true, ".htm": true, ".xhtml": true, ".shtml": true,
	".mhtml": true, ".mht": true, ".svg": true,
}

// contentSniffLen is how much of a file http.DetectContentType reads. Passing
// more is harmless but pointless.
const contentSniffLen = 512

// ClassifyAttachment decides the MIME type an upload is stored under, from the
// first bytes of its content and its filename. It returns an error describing
// why a file is refused, phrased for the person who tried to send it.
//
// head may be shorter than contentSniffLen; an empty file classifies as text.
func ClassifyAttachment(filename string, head []byte) (string, error) {
	ext := attachmentExt(filename)
	if DangerousExtensions[ext] || refusedMarkupExtensions[ext] {
		return "", fmt.Errorf("files with a %s extension are not accepted", ext)
	}

	if len(head) > contentSniffLen {
		head = head[:contentSniffLen]
	}
	detected := http.DetectContentType(head)
	if idx := strings.Index(detected, ";"); idx >= 0 {
		detected = strings.TrimSpace(detected[:idx])
	}

	// A known text-like extension decides the stored type, but only once the
	// bytes agree it is text: the extension is the uploader's word, and a
	// binary payload wearing a .json name should not become an attachment the
	// UI offers to preview as source.
	if textLikeExtensions[ext] {
		if !isTextContentType(detected, head) {
			return "", fmt.Errorf("%s content does not look like text", ext)
		}
		if ext == ".md" {
			return "text/markdown", nil
		}
		return "text/plain", nil
	}

	// Everything else is what the bytes say it is, checked against the
	// deny-list. text/html and application/javascript are refused, so a
	// sniffed HTML or script body is refused here whatever it is called — which
	// is the other half of the markup rule above: the extension check catches
	// the payloads the sniff misses, the sniff catches the ones renamed .txt.
	if IsDangerousMimeType(detected) {
		return "", fmt.Errorf("file type %q is not accepted", detected)
	}
	return detected, nil
}

// isTextContentType reports whether a sniffed type means "these bytes are
// text". http.DetectContentType answers text/plain for ordinary text and
// text/html or text/xml when the content opens with markup — a Markdown file
// starting with a tag, say. All of them are text; which of them a text-like
// extension is stored as is decided by the extension, not by this.
func isTextContentType(detected string, head []byte) bool {
	if strings.HasPrefix(detected, "text/") {
		return true
	}
	// application/octet-stream is Go's http.DetectContentType fallback
	// for "I don't know" — it fires for valid UTF-8 containing control
	// characters the sniffer doesn't expect (e.g. vertical tab 0x0B).
	// Since the caller already confirmed a text-like extension, treat
	// "I don't know" as text unless the content contains null bytes,
	// which are characteristic of genuinely binary files.
	if detected == "application/octet-stream" {
		for _, b := range head {
			if b == 0 {
				return false
			}
		}
		return true
	}
	return false
}

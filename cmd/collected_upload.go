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
	"fmt"
	"io"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// signedURLUploader uploads content to a signed URL. The skill, template
// and harness-config services implement it.
type signedURLUploader interface {
	UploadFile(ctx context.Context, url string, method string, headers map[string]string, content io.Reader) error
}

// uploadCollectedFiles uploads collected files to their signed URLs. For
// each target, in order, it reads the collected entry byPath[target.Path]
// through root with transfer.ReadCollectedFile and uploads the verified
// bytes. root must be opened at the directory the entries were collected
// from.
//
// A target with no collected entry is passed to missing (if non-nil) and
// skipped; sent (if non-nil) is called after each successful upload. The
// first read or upload error stops the upload and is returned: a file that
// fails verification is not sent, and no later file is sent.
func uploadCollectedFiles(ctx context.Context, up signedURLUploader, root *os.Root, byPath map[string]transfer.FileInfo, targets []hubclient.UploadURLInfo, missing, sent func(path string)) error {
	for _, target := range targets {
		fi, ok := byPath[target.Path]
		if !ok {
			if missing != nil {
				missing(target.Path)
			}
			continue
		}
		data, err := transfer.ReadCollectedFile(root, fi)
		if err != nil {
			return err
		}
		if err := up.UploadFile(ctx, target.URL, target.Method, target.Headers, bytes.NewReader(data)); err != nil {
			return fmt.Errorf("failed to upload %s: %w", target.Path, err)
		}
		if sent != nil {
			sent(target.Path)
		}
	}
	return nil
}

// collectedByPath indexes collected entries by their relative path.
func collectedByPath(files []transfer.FileInfo) map[string]transfer.FileInfo {
	m := make(map[string]transfer.FileInfo, len(files))
	for _, f := range files {
		m[f.Path] = f
	}
	return m
}

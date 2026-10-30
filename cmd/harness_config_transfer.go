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
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// verifyHarnessConfigArtifactHash checks the SHA-256 hash of downloaded
// content against the hash announced by the Hub for this file. The Hub may
// omit the hash for legacy artifacts; in that case verification is skipped
// so historical bundles continue to install (operators can detect missing
// per-file hashes by inspecting the manifest's overall ContentHash).
func verifyHarnessConfigArtifactHash(file hubclient.DownloadURLInfo, content []byte) error {
	if file.Hash == "" {
		return nil
	}
	got := transfer.HashBytes(content)
	if got != file.Hash {
		return fmt.Errorf(
			"hash mismatch for %s: hub announced %s but downloaded content is %s",
			file.Path, file.Hash, got,
		)
	}
	return nil
}

func downloadHarnessConfigContent(
	ctx context.Context,
	service hubclient.HarnessConfigService,
	harnessConfigID string,
	file hubclient.DownloadURLInfo,
	useHubFileRead bool,
) ([]byte, error) {
	if useHubFileRead {
		content, err := service.ReadFile(ctx, harnessConfigID, file.Path)
		if err != nil {
			return nil, fmt.Errorf("read harness config file through Hub API: %w", err)
		}
		return content, nil
	}

	content, err := service.DownloadFile(ctx, file.URL)
	if err != nil {
		return nil, fmt.Errorf("download harness config file: %w", err)
	}
	return content, nil
}

// uploadHarnessConfigFiles uploads the collected harness-config files in
// filesToUpload. root must be opened at the collected directory; every file
// is read through it and verified against its collected size and hash with
// transfer.ReadCollectedFile, so a file that changed after collection stops
// the upload before any of its bytes are sent.
func uploadHarnessConfigFiles(
	ctx context.Context,
	service hubclient.HarnessConfigService,
	harnessConfigID string,
	root *os.Root,
	localFileMap map[string]*hubclient.FileInfo,
	filesToUpload []hubclient.FileUploadRequest,
	uploadURLs []hubclient.UploadURLInfo,
) error {
	if hasLocalSignedURLs(uploadURLs) {
		return uploadHarnessConfigFilesThroughHubAPI(ctx, service, harnessConfigID, root, localFileMap, filesToUpload)
	}

	return uploadHarnessConfigFilesBySignedURL(ctx, service, root, localFileMap, uploadURLs, "Uploaded", true)
}

func uploadHarnessConfigFilesThroughHubAPI(
	ctx context.Context,
	service hubclient.HarnessConfigService,
	harnessConfigID string,
	root *os.Root,
	localFileMap map[string]*hubclient.FileInfo,
	filesToUpload []hubclient.FileUploadRequest,
) error {
	filesForFallback := make([]hubclient.FileInfo, 0, len(filesToUpload))
	for _, req := range filesToUpload {
		fileInfo := localFileMap[req.Path]
		if fileInfo == nil {
			fmt.Printf("  Warning: no matching file for %s\n", req.Path)
			continue
		}
		filesForFallback = append(filesForFallback, *fileInfo)
	}

	if err := service.UploadFilesMultipart(ctx, harnessConfigID, root, filesForFallback); err != nil {
		return fmt.Errorf("upload harness config files through Hub API: %w", err)
	}

	for _, fileInfo := range filesForFallback {
		fmt.Printf("  Uploaded: %s\n", fileInfo.Path)
	}
	return nil
}

// uploadHarnessConfigFilesBySignedURL uploads the collected files that have
// an upload URL, read through root, printing "  <verb>: <path>" for each.
// With warnMissing it also prints a warning for a URL with no matching file.
func uploadHarnessConfigFilesBySignedURL(
	ctx context.Context,
	service hubclient.HarnessConfigService,
	root *os.Root,
	localFileMap map[string]*hubclient.FileInfo,
	uploadURLs []hubclient.UploadURLInfo,
	verb string,
	warnMissing bool,
) error {
	byPath := make(map[string]transfer.FileInfo, len(localFileMap))
	for p, fi := range localFileMap {
		if fi != nil {
			byPath[p] = *fi
		}
	}
	var missing func(string)
	if warnMissing {
		missing = func(path string) { fmt.Printf("  Warning: no matching file for %s\n", path) }
	}
	return uploadCollectedFiles(ctx, service, root, byPath, uploadURLs, missing, func(path string) {
		fmt.Printf("  %s: %s\n", verb, path)
	})
}

// writeHarnessConfigFile writes a pulled harness-config file through
// writePulledFile, with the mode the Hub recorded for it.
func writeHarnessConfigFile(root *os.Root, relPath string, content []byte, mode string) error {
	if err := writePulledFile(root, relPath, content, mode); err != nil {
		return fmt.Errorf("write harness config file: %w", err)
	}
	return nil
}

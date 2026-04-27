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
	"path/filepath"

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

func uploadHarnessConfigFiles(
	ctx context.Context,
	service hubclient.HarnessConfigService,
	harnessConfigID string,
	localFileMap map[string]*hubclient.FileInfo,
	filesToUpload []hubclient.FileUploadRequest,
	uploadURLs []hubclient.UploadURLInfo,
) error {
	if hasLocalSignedURLs(uploadURLs) {
		return uploadHarnessConfigFilesThroughHubAPI(ctx, service, harnessConfigID, localFileMap, filesToUpload)
	}

	return uploadHarnessConfigFilesBySignedURL(ctx, service, localFileMap, uploadURLs)
}

func uploadHarnessConfigFilesThroughHubAPI(
	ctx context.Context,
	service hubclient.HarnessConfigService,
	harnessConfigID string,
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

	if err := service.UploadFilesMultipart(ctx, harnessConfigID, filesForFallback); err != nil {
		return fmt.Errorf("upload harness config files through Hub API: %w", err)
	}

	for _, fileInfo := range filesForFallback {
		fmt.Printf("  Uploaded: %s\n", fileInfo.Path)
	}
	return nil
}

func uploadHarnessConfigFilesBySignedURL(
	ctx context.Context,
	service hubclient.HarnessConfigService,
	localFileMap map[string]*hubclient.FileInfo,
	uploadURLs []hubclient.UploadURLInfo,
) error {
	for _, urlInfo := range uploadURLs {
		fileInfo := localFileMap[urlInfo.Path]
		if fileInfo == nil {
			fmt.Printf("  Warning: no matching file for %s\n", urlInfo.Path)
			continue
		}

		if err := uploadHarnessConfigFileBySignedURL(ctx, service, fileInfo, urlInfo); err != nil {
			return err
		}
		fmt.Printf("  Uploaded: %s\n", fileInfo.Path)
	}
	return nil
}

func uploadHarnessConfigFileBySignedURL(
	ctx context.Context,
	service hubclient.HarnessConfigService,
	fileInfo *hubclient.FileInfo,
	urlInfo hubclient.UploadURLInfo,
) (err error) {
	f, err := os.Open(fileInfo.FullPath)
	if err != nil {
		return fmt.Errorf("open harness config file for upload: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close harness config file: %w", closeErr)
		}
	}()

	if err := service.UploadFile(ctx, urlInfo.URL, urlInfo.Method, urlInfo.Headers, f); err != nil {
		return fmt.Errorf("upload harness config file: %w", err)
	}
	return nil
}

func ensureParentDir(filePath string) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return fmt.Errorf("create harness config destination directory: %w", err)
	}
	return nil
}

func writeHarnessConfigFile(filePath string, content []byte) error {
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return fmt.Errorf("write harness config file: %w", err)
	}
	return nil
}

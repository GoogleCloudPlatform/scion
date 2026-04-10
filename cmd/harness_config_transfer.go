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
)

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
) error {
	f, err := os.Open(fileInfo.FullPath)
	if err != nil {
		return fmt.Errorf("open harness config file for upload: %w", err)
	}
	defer f.Close()

	if err := service.UploadFile(ctx, urlInfo.URL, urlInfo.Method, urlInfo.Headers, f); err != nil {
		return fmt.Errorf("upload harness config file: %w", err)
	}
	return nil
}

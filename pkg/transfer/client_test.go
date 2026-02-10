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

package transfer

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewClient(t *testing.T) {
	// With nil HTTP client
	client := NewClient(nil)
	if client.HTTPClient != http.DefaultClient {
		t.Error("expected DefaultClient when nil is passed")
	}

	// With custom HTTP client
	customClient := &http.Client{}
	client = NewClient(customClient)
	if client.HTTPClient != customClient {
		t.Error("expected custom client to be used")
	}
}

func TestClient_UploadFile_FileURL(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "subdir", "test.txt")

	client := NewClient(nil)
	content := bytes.NewReader([]byte("hello world"))

	url := UploadURLInfo{
		Path:   "test.txt",
		URL:    "file://" + destPath,
		Method: "PUT",
	}

	err := client.UploadFile(context.Background(), url, content)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	// Verify file was written
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read uploaded file: %v", err)
	}

	if string(data) != "hello world" {
		t.Errorf("unexpected content: %s", string(data))
	}
}

func TestClient_UploadFile_HTTP(t *testing.T) {
	var receivedContent []byte
	var receivedMethod string
	var receivedHeaders map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedHeaders = make(map[string]string)
		for k, v := range r.Header {
			if len(v) > 0 {
				receivedHeaders[k] = v[0]
			}
		}
		receivedContent, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	content := bytes.NewReader([]byte("test content"))

	url := UploadURLInfo{
		Path:    "test.txt",
		URL:     server.URL + "/upload",
		Method:  "PUT",
		Headers: map[string]string{"X-Custom": "value"},
	}

	err := client.UploadFile(context.Background(), url, content)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	if receivedMethod != "PUT" {
		t.Errorf("expected PUT method, got %s", receivedMethod)
	}

	if receivedHeaders["X-Custom"] != "value" {
		t.Error("custom header not received")
	}

	if string(receivedContent) != "test content" {
		t.Errorf("unexpected content: %s", string(receivedContent))
	}
}

func TestClient_UploadFile_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("access denied"))
	}))
	defer server.Close()

	client := NewClient(server.Client())
	content := bytes.NewReader([]byte("test"))

	url := UploadURLInfo{
		Path:   "test.txt",
		URL:    server.URL + "/upload",
		Method: "PUT",
	}

	err := client.UploadFile(context.Background(), url, content)
	if err == nil {
		t.Error("expected error for 403 response")
	}
}

func TestClient_DownloadFile_FileURL(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	content := []byte("download me")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatalf("failed to create source file: %v", err)
	}

	client := NewClient(nil)
	data, err := client.DownloadFile(context.Background(), "file://"+srcPath)
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}

	if string(data) != "download me" {
		t.Errorf("unexpected content: %s", string(data))
	}
}

func TestClient_DownloadFile_HTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("server content"))
	}))
	defer server.Close()

	client := NewClient(server.Client())
	data, err := client.DownloadFile(context.Background(), server.URL+"/file.txt")
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}

	if string(data) != "server content" {
		t.Errorf("unexpected content: %s", string(data))
	}
}

func TestClient_DownloadFile_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer server.Close()

	client := NewClient(server.Client())
	_, err := client.DownloadFile(context.Background(), server.URL+"/missing.txt")
	if err == nil {
		t.Error("expected error for 404 response")
	}
}

func TestClient_DownloadToFile(t *testing.T) {
	dir := t.TempDir()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("file content"))
	}))
	defer server.Close()

	destPath := filepath.Join(dir, "subdir", "downloaded.txt")
	client := NewClient(server.Client())

	err := client.DownloadToFile(context.Background(), server.URL+"/file.txt", destPath)
	if err != nil {
		t.Fatalf("DownloadToFile failed: %v", err)
	}

	// Verify file was created
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}

	if string(data) != "file content" {
		t.Errorf("unexpected content: %s", string(data))
	}
}

func TestClient_UploadFiles(t *testing.T) {
	dir := t.TempDir()

	// Create source files
	file1 := filepath.Join(dir, "file1.txt")
	file2 := filepath.Join(dir, "file2.txt")
	if err := os.WriteFile(file1, []byte("content1"), 0644); err != nil {
		t.Fatalf("failed to create file1: %v", err)
	}
	if err := os.WriteFile(file2, []byte("content2"), 0644); err != nil {
		t.Fatalf("failed to create file2: %v", err)
	}

	uploadDir := filepath.Join(dir, "uploads")

	files := []FileInfo{
		{Path: "file1.txt", FullPath: file1, Size: 8},
		{Path: "file2.txt", FullPath: file2, Size: 8},
	}

	urls := []UploadURLInfo{
		{Path: "file1.txt", URL: "file://" + filepath.Join(uploadDir, "file1.txt"), Method: "PUT"},
		{Path: "file2.txt", URL: "file://" + filepath.Join(uploadDir, "file2.txt"), Method: "PUT"},
	}

	client := NewClient(nil)
	var uploadedFiles []string
	progress := func(file FileInfo, bytesTransferred int64) error {
		uploadedFiles = append(uploadedFiles, file.Path)
		return nil
	}

	err := client.UploadFiles(context.Background(), files, urls, progress)
	if err != nil {
		t.Fatalf("UploadFiles failed: %v", err)
	}

	if len(uploadedFiles) != 2 {
		t.Errorf("expected 2 files uploaded, got %d", len(uploadedFiles))
	}

	// Verify files were uploaded
	for _, url := range urls {
		path := url.URL[7:] // Strip file://
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("file not uploaded: %s", path)
		}
	}
}

func TestClient_DownloadFiles(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dest")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file1.txt":
			w.Write([]byte("content1"))
		case "/file2.txt":
			w.Write([]byte("content2"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	urls := []DownloadURLInfo{
		{Path: "file1.txt", URL: server.URL + "/file1.txt", Size: 8},
		{Path: "file2.txt", URL: server.URL + "/file2.txt", Size: 8},
	}

	client := NewClient(server.Client())
	var downloadedFiles []string
	progress := func(file FileInfo, bytesTransferred int64) error {
		downloadedFiles = append(downloadedFiles, file.Path)
		return nil
	}

	err := client.DownloadFiles(context.Background(), urls, destDir, progress)
	if err != nil {
		t.Fatalf("DownloadFiles failed: %v", err)
	}

	if len(downloadedFiles) != 2 {
		t.Errorf("expected 2 files downloaded, got %d", len(downloadedFiles))
	}

	// Verify files were downloaded
	for i, url := range urls {
		path := filepath.Join(destDir, url.Path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("file not downloaded: %s", path)
			continue
		}
		expected := "content" + string('1'+rune(i))
		if string(data) != expected {
			t.Errorf("unexpected content in %s: %s", url.Path, string(data))
		}
	}
}

func TestClient_UploadFileWithMethod_DefaultMethod(t *testing.T) {
	var receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	content := bytes.NewReader([]byte("test"))

	// Empty method should default to PUT
	err := client.UploadFileWithMethod(context.Background(), server.URL+"/upload", "", nil, content)
	if err != nil {
		t.Fatalf("UploadFileWithMethod failed: %v", err)
	}

	if receivedMethod != "PUT" {
		t.Errorf("expected default PUT method, got %s", receivedMethod)
	}
}

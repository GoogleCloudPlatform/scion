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

// Package gcp provides shared GCP utility functions used by both the hub server
// and sciontool for extracting metadata from GCP service account credentials.
package gcp

import (
	"encoding/json"
	"os"
)

// ExtractProjectID reads a GCP service account JSON key file and returns the
// project_id field. Returns empty string on any error (file not found, invalid
// JSON, missing field).
func ExtractProjectID(credentialsPath string) string {
	if credentialsPath == "" {
		return ""
	}
	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		return ""
	}
	return ParseProjectID(data)
}

// ParseProjectID extracts the project_id field from raw GCP service account
// JSON key data. Returns empty string if the data is not valid JSON or lacks
// a project_id field.
func ParseProjectID(data []byte) string {
	var creds struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	return creds.ProjectID
}

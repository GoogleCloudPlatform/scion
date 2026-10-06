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

package agent

// readSharedDirStorageRecord returns the recorded default shared-dir
// storage backend for the agent whose directory is agentDir, or "" when
// none is recorded. See loadSharedDirStorageRecord.
func readSharedDirStorageRecord(agentDir string) (string, error) {
	rec, err := loadSharedDirStorageRecord(agentDir)
	if err != nil || rec == nil {
		return "", err
	}
	return rec.Backend, nil
}

// writeSharedDirStorageRecord records backend for every shared dir of the
// agent whose directory is agentDir, through writeAgentRecordFile.
func writeSharedDirStorageRecord(agentDir, backend string) error {
	return saveSharedDirStorageRecord(agentDir, &sharedDirStorageRecord{Backend: backend})
}

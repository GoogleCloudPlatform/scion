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

package runtimebroker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	dispatchAttemptInProgress = "in_progress"
	dispatchAttemptSucceeded  = "succeeded"
	dispatchAttemptFailed     = "failed"

	dispatchAttemptTTL = 24 * time.Hour
)

func (s *Server) initStateStore() error {
	if s.stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dispatchAttemptDir(), 0755); err != nil {
		return fmt.Errorf("create dispatch attempt dir: %w", err)
	}
	if err := s.loadDispatchAttempts(); err != nil {
		return fmt.Errorf("load dispatch attempts: %w", err)
	}
	return nil
}

func (s *Server) dispatchAttemptDir() string {
	return filepath.Join(s.stateDir, "dispatch-attempts")
}

func (s *Server) dispatchAttemptPath(requestID string) string {
	return filepath.Join(s.dispatchAttemptDir(), requestID+".json")
}

func (s *Server) loadDispatchAttempts() error {
	entries, err := os.ReadDir(s.dispatchAttemptDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(s.dispatchAttemptDir(), e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var a dispatchAttempt
		if err := json.Unmarshal(raw, &a); err != nil {
			continue
		}
		if a.RequestID == "" {
			a.RequestID = strings.TrimSuffix(e.Name(), ".json")
		}
		if a.UpdatedAt.IsZero() {
			a.UpdatedAt = a.CreatedAt
		}
		if now.Sub(a.UpdatedAt) > dispatchAttemptTTL {
			_ = os.Remove(p)
			continue
		}
		s.dispatchAttempts[a.RequestID] = &a
	}
	return nil
}

func (s *Server) writeJSONFile(path string, v interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) beginCreateAttempt(requestID, agentID string) (*dispatchAttempt, *dispatchAttempt) {
	if requestID == "" {
		return nil, nil
	}
	now := time.Now()
	for id, a := range s.dispatchAttempts {
		if now.Sub(a.UpdatedAt) > dispatchAttemptTTL {
			delete(s.dispatchAttempts, id)
			if s.stateDir != "" {
				_ = os.Remove(s.dispatchAttemptPath(id))
			}
		}
	}
	if existing, ok := s.dispatchAttempts[requestID]; ok {
		return nil, existing
	}
	a := &dispatchAttempt{
		RequestID: requestID,
		Operation: "create",
		AgentID:   agentID,
		Status:    dispatchAttemptInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.dispatchAttempts[requestID] = a
	if s.stateDir != "" {
		_ = s.writeJSONFile(s.dispatchAttemptPath(requestID), a)
	}
	return a, nil
}

func (s *Server) completeAttempt(a *dispatchAttempt, status string, httpStatus int, createResp *CreateAgentResponse, envResp *EnvRequirementsResponse, errMsg string) {
	if a == nil {
		return
	}
	a.Status = status
	a.HTTPStatus = httpStatus
	a.CreatedResponse = createResp
	a.EnvResponse = envResp
	a.Error = errMsg
	a.UpdatedAt = time.Now()
	s.dispatchAttempts[a.RequestID] = a
	if s.stateDir != "" {
		_ = s.writeJSONFile(s.dispatchAttemptPath(a.RequestID), a)
	}
}

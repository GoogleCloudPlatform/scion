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

package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

var bearerPattern = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)?bearer\s+[^\s,;]+`)

type credentialRedactor struct {
	mu        sync.RWMutex
	forbidden []string
}

func newCredentialRedactor(credentials ...string) *credentialRedactor {
	redactor := &credentialRedactor{}
	redactor.add(credentials...)
	return redactor
}

func (r *credentialRedactor) add(credentials ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, credential := range credentials {
		if credential == "" {
			continue
		}
		digest := sha256.Sum256([]byte(credential))
		r.forbidden = append(r.forbidden,
			credential,
			hex.EncodeToString(digest[:]),
			base64.StdEncoding.EncodeToString(digest[:]),
			base64.RawURLEncoding.EncodeToString(digest[:]),
		)
	}
}

func (r *credentialRedactor) redact(input string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	redacted := bearerPattern.ReplaceAllString(input, "Authorization: Bearer [REDACTED]")
	for _, forbidden := range r.forbidden {
		redacted = strings.ReplaceAll(redacted, forbidden, "[REDACTED]")
	}
	return redacted
}

type sanitizedWriter struct {
	mu       sync.Mutex
	redactor *credentialRedactor
	buffer   bytes.Buffer
	pending  bytes.Buffer
}

func newSanitizedWriter(redactor *credentialRedactor) *sanitizedWriter {
	return &sanitizedWriter{redactor: redactor}
}

func (w *sanitizedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.pending.Write(data)
	for {
		line, err := w.pending.ReadString('\n')
		if err != nil {
			_, _ = w.pending.WriteString(line)
			break
		}
		_, _ = w.buffer.WriteString(w.redactor.redact(line))
	}
	return len(data), nil
}

func (w *sanitizedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String() + w.redactor.redact(w.pending.String())
}

// observation intentionally contains only the fields approved for the #1620
// integration evidence. Credential material has no representable field.
type observation struct {
	RequestID      string  `json:"request_id,omitempty"`
	ReplicaID      string  `json:"replica_id,omitempty"`
	Outcome        string  `json:"outcome,omitempty"`
	CacheStatus    string  `json:"cache_status,omitempty"`
	TaskID         string  `json:"task_id,omitempty"`
	ContextID      string  `json:"context_id,omitempty"`
	Cursor         string  `json:"cursor,omitempty"`
	PrincipalLabel string  `json:"principal_label,omitempty"`
	LatencyMS      float64 `json:"latency_ms,omitempty"`
}

type observationRecorder struct {
	output *sanitizedWriter
}

func newObservationRecorder(redactor *credentialRedactor) *observationRecorder {
	return &observationRecorder{output: newSanitizedWriter(redactor)}
}

func (r *observationRecorder) record(value observation) {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	_, _ = r.output.Write(append(encoded, '\n'))
}

func (r *observationRecorder) String() string {
	return r.output.String()
}

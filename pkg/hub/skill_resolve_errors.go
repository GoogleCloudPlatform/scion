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
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Skill resolve errors return a fixed reason phrase and status; the full
// error goes to the server-side log (ptone/scion#5079). Each
// ResolveSkillError carries the fixed phrase for its code, the upstream
// HTTP status when one is known, and a correlation id that also appears in
// the log record holding the full error.

// skillResolveFailureLogMsg is the message of the log record that carries
// the full error of a skill resolve failure.
const skillResolveFailureLogMsg = "Skill resolve failed"

// Sources of a skill resolve failure, for the log record.
const (
	skillResolveSourceFederation = "federation"
)

// skillResolveCorrelationID returns the id that ties a skill resolve error
// to its log record: the request id of ctx when there is one (the
// X-Request-ID the request log middleware sets), else a new random id,
// when there is no request id (for example dispatch outside a request).
func skillResolveCorrelationID(ctx context.Context) string {
	if id := logging.RequestIDFromContext(ctx); id != "" {
		return id
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// newSkillResolveError logs the full error detail of a skill resolve
// failure for the declared reference uri, and returns the error to report:
// the fixed reason phrase for code, code, the upstream HTTP status (0 when
// none is known) and the correlation id of the log record. source names
// the resolution path; attrs are extra log attributes. The reference is
// logged in its redacted form, and detail is withheld from the log when
// the reference is redacted (see agent.ResolverReasonForLog).
func newSkillResolveError(ctx context.Context, source, uri, code string, status int, detail string, attrs ...any) *ResolveSkillError {
	id := skillResolveCorrelationID(ctx)
	slog.WarnContext(ctx, skillResolveFailureLogMsg, append(attrs,
		"correlation_id", id,
		"source", source,
		"skill_uri", agent.LoggedSkillRef(uri),
		"code", code,
		"status", status,
		"error", agent.ResolverReasonForLog(uri, detail))...)
	return &ResolveSkillError{
		URI:           uri,
		Code:          code,
		Message:       agent.SkillResolutionPhrase(code),
		Status:        status,
		CorrelationID: id,
	}
}

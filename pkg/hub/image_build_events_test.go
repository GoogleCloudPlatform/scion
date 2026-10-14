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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageBuildSubject_IsUserScoped(t *testing.T) {
	assert.Equal(t, "user.u-1.images.job-1", imageBuildSubject("u-1", "job-1"))
}

// TestRunImageBuild_DeliversOnlyToRequesterSubject runs a stub build script
// and checks that its output reaches a subscriber on the requesting user's
// subject, and not a subscriber for another user or one on system.images.
func TestRunImageBuild_DeliversOnlyToRequesterSubject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub build script is a POSIX shell script")
	}
	script := writeStubBuildScript(t, "#!/bin/sh\necho build-line-one\necho build-line-two\n")

	pub := NewChannelEventPublisher()
	requesterCh, unsubRequester := pub.Subscribe("user.u-req.>")
	defer unsubRequester()
	otherCh, unsubOther := pub.Subscribe("user.u-other.>")
	defer unsubOther()
	systemCh, unsubSystem := pub.Subscribe("system.>")
	defer unsubSystem()

	srv := &Server{events: pub}
	srv.runImageBuild(context.Background(), imageBuildSubject("u-req", "job-1"), script, []string{"claude"})

	var lines []string
	var sawDone bool
	for drained := false; !drained; {
		select {
		case evt := <-requesterCh:
			assert.Equal(t, "user.u-req.images.job-1", evt.Subject)
			var payload map[string]string
			require.NoError(t, json.Unmarshal(evt.Data, &payload))
			if payload["type"] == "log" {
				lines = append(lines, payload["line"])
			}
			if payload["status"] == "done" {
				sawDone = true
			}
		default:
			drained = true
		}
	}
	assert.Equal(t, []string{"build-line-one", "build-line-two", "build complete"}, lines,
		"the requester must receive the build output")
	assert.True(t, sawDone, "the requester must receive the per-image completion event")

	assert.Empty(t, otherCh, "a different user's subscription must not receive build output")
	assert.Empty(t, systemCh, "build output must not be published on system.* subjects")
}

// writeStubBuildScript writes an executable stub build script, closing it
// before it is run. It skips the test when the temp directory does not allow
// exec (a noexec mount), and waits out a transient ETXTBSY, which a fork in
// another goroutine can cause while the file was still open for writing.
func writeStubBuildScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "build-images.sh")
	f, err := os.OpenFile(script, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	require.NoError(t, err)
	_, err = f.WriteString(body)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	for attempt := 0; ; attempt++ {
		err := exec.Command(script).Run()
		switch {
		case err == nil:
			return script
		case errors.Is(err, syscall.ETXTBSY) && attempt < 50:
			time.Sleep(20 * time.Millisecond)
		case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
			t.Skipf("cannot exec a stub script in %s (temp directory likely mounted noexec): %v", dir, err)
		default:
			t.Fatalf("stub build script %s did not run: %v", script, err)
		}
	}
}

// TestHandleSystemImagesBuild_RequiresUserIdentity checks that a build
// request without a user identity (none at all, or a non-user principal such
// as an agent or a broker) is refused before a build is marked active, since
// there is no requester subject to deliver its output to.
func TestHandleSystemImagesBuild_RequiresUserIdentity(t *testing.T) {
	agentClaims := &AgentTokenClaims{}
	agentClaims.Subject = "agent-1"

	tests := []struct {
		name     string
		identity Identity
	}{
		{name: "no identity"},
		{name: "agent identity", identity: &agentIdentityWrapper{agentClaims}},
		{name: "broker identity", identity: NewBrokerIdentity("broker-1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{events: noopEventPublisher{}}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/system/images/build", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			if tt.identity != nil {
				req = req.WithContext(contextWithIdentity(req.Context(), tt.identity))
			}
			rr := httptest.NewRecorder()

			srv.handleSystemImagesBuild(rr, req)

			assert.Equal(t, http.StatusUnauthorized, rr.Code, rr.Body.String())
			assert.False(t, srv.imageBuildActive.Load(), "a refused request must not mark a build active")
		})
	}
}

// TestAuthorizeSSESubjects_ImageBuildSubjectIsRequesterOnly checks that the
// image-build subject is admitted for the requesting user's session and
// denied for any other user's session.
func TestAuthorizeSSESubjects_ImageBuildSubjectIsRequesterOnly(t *testing.T) {
	ws := &WebServer{authzService: NewAuthzService(&mockAuthzStore{}, nil)}
	subject := imageBuildSubject("user-1", "job-1")

	requester := httptest.NewRequest("GET", "/events", nil)
	requester = requester.WithContext(context.WithValue(requester.Context(), webUserContextKey{},
		&webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "admin"}))
	assert.Empty(t, ws.authorizeSSESubjects(requester, []string{subject}),
		"the requesting user's session must be allowed to subscribe")

	other := httptest.NewRequest("GET", "/events", nil)
	other = other.WithContext(context.WithValue(other.Context(), webUserContextKey{},
		&webSessionUser{UserID: "user-2", Email: "c@d.com", Role: "admin"}))
	assert.Equal(t, []string{subject}, ws.authorizeSSESubjects(other, []string{subject}),
		"another user's session, admin or not, must be denied")
}

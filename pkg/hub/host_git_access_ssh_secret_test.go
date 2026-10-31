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

//go:build !no_sqlite

package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every ssh clone URL is refused on a hub-host git path: with a project git
// credential the text says the credential authenticates https only, without
// one that ssh is not supported. https is refused by neither.
func TestSSHCloneURLRefused(t *testing.T) {
	for _, u := range []string{
		"ssh://git@git.example.test/org/repo.git",
		"git+ssh://git@git.example.test/org/repo.git",
		"ssh+git://git@git.example.test/org/repo.git",
		"git@git.example.test:org/repo.git",
		// Refused earlier by the host clone-URL check on every hub path, and
		// classified as ssh here too, so a relaxed check would not let them
		// through.
		"SSH://git@git.example.test/org/repo.git",
		"GIT+SSH://git@git.example.test/org/repo.git",
		"git.example.test:org/repo.git",
	} {
		err := sshCloneURLRefused(u, true)
		require.ErrorIs(t, err, errSSHCloneURLProjectSecretHTTPSOnly, u)
		assert.Equal(t, sshSecretHTTPSOnlyText, err.Error(), u)

		err = sshCloneURLRefused(u, false)
		require.ErrorIs(t, err, errSSHCloneURLUnsupported, u)
		assert.Equal(t, sshUnsupportedText, err.Error(), u)
	}
	for _, haveCredential := range []bool{false, true} {
		assert.NoError(t, sshCloneURLRefused("https://git.example.test/org/repo.git", haveCredential))
		assert.NoError(t, sshCloneURLRefused("git://git.example.test/org/repo.git", haveCredential))
	}
}

// With a project git secret, an https clone or pull of the project's
// recorded remote still goes ahead to git and offers the token: the refusal
// is scoped to ssh.
func TestHostGitAccess_HTTPSWithProjectSecretProceeds(t *testing.T) {
	tr := newHostGitTrace(t)

	t.Run("local clone", func(t *testing.T) {
		srv, st, setToken := hostGitTokenServer(t)
		p := hostGitProject(t, st, "host-git-https-secret-clone", tr.url("org/repo"), "")
		setToken(p.ID)
		m := tr.mark()
		err := srv.cloneSharedWorkspaceProject(context.Background(), p)
		require.Error(t, err, "the 401 endpoint cannot be cloned")
		assert.NotEqual(t, sshSecretHTTPSOnlyText, err.Error())
		assert.NotEqual(t, sshUnsupportedText, err.Error())
		tr.assertTokenOnly(t, m)
	})

	t.Run("pull", func(t *testing.T) {
		srv, st, setToken := hostGitTokenServer(t)
		p := hostGitProvisionedProject(t, srv, st, "host-git-https-secret-pull", tr.url("org/repo"), "")
		setToken(p.ID)
		m := tr.mark()
		code, body := hostGitPull(t, srv, p.ID)
		require.NotEqual(t, http.StatusOK, code, "the 401 endpoint cannot be pulled: %s", body)
		assert.NotContains(t, body, sshSecretHTTPSOnlyText)
		assert.NotContains(t, body, sshUnsupportedText)
		tr.assertTokenOnly(t, m)
	})
}

// Project create and project clone on the local backend refuse a
// shared-workspace project's ssh clone URL (422, fixed text) before git
// runs, with or without a git credential (here the creator's token for a
// remote on the GitHub host).
func TestProjectCreateAndClone_LocalSSHRefused(t *testing.T) {
	const remote = "github.com/team/repo"
	labels := []struct{ name, label string }{
		{"ssh", "ssh://git@github.com/team/repo.git"},
		{"scp", "git@github.com:team/repo.git"},
	}
	for i, lc := range labels {
		for _, withSecret := range []bool{false, true} {
			want := sshUnsupportedText
			if withSecret {
				want = sshSecretHTTPSOnlyText
			}
			t.Run(fmt.Sprintf("%s/secret=%t", lc.name, withSecret), func(t *testing.T) {
				record := fakeSSH(t)
				gitLog := hostGitFakeGit(t)
				identityTestHome(t)
				srv, st := testServer(t)
				require.False(t, srv.workspaceBackendShared())
				token := ""
				if withSecret {
					token = "test-creator-token"
				}
				srv.SetSecretBackend(&userTokenSecretBackend{value: token})

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
					Name: fmt.Sprintf("SSH Create %d %t", i, withSecret), GitRemote: remote,
					WorkspaceMode: store.WorkspaceModeShared,
					Labels:        map[string]string{store.LabelCloneURL: lc.label},
				})
				require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "create: %s", rec.Body.String())
				assert.Equal(t, want, decodeErrorMessage(t, rec.Body.Bytes()), "create")

				src := &store.Project{
					ID:        tid(fmt.Sprintf("ssh-clone-src-%d-%t", i, withSecret)),
					Name:      fmt.Sprintf("SSH Clone Source %d %t", i, withSecret),
					Slug:      fmt.Sprintf("ssh-clone-source-%d-%t", i, withSecret),
					GitRemote: remote,
					OwnerID:   DevUserID,
					CreatedBy: DevUserID,
					Labels: map[string]string{
						store.LabelWorkspaceMode: store.WorkspaceModeShared,
						store.LabelCloneURL:      lc.label,
					},
				}
				require.NoError(t, st.CreateProject(context.Background(), src))
				rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
					map[string]string{"name": fmt.Sprintf("SSH Clone %d %t", i, withSecret)})
				require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "clone: %s", rec.Body.String())
				assert.Equal(t, want, decodeErrorMessage(t, rec.Body.Bytes()), "clone")

				assert.False(t, sshRan(t, record), "git must not reach ssh")
				assert.Empty(t, hostGitInvocations(t, gitLog), "git must not run")
			})
		}
	}
}

// An agent dispatch whose provisioning returns either ssh refusal answers
// 422 with its fixed text, as project create does.
func TestDispatchCreateErrorResponse_SSHRefusals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errSSHCloneURLUnsupported, sshUnsupportedText},
		{errSSHCloneURLProjectSecretHTTPSOnly, sshSecretHTTPSOnlyText},
	} {
		rec := httptest.NewRecorder()
		dispatchCreateErrorResponse(rec, fmt.Errorf("dispatch: %w", tc.err), "agent-ssh-refusal")
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, tc.want)
		assert.Equal(t, tc.want, decodeErrorMessage(t, rec.Body.Bytes()))
		assert.Equal(t, ErrCodeInvalidRequest, decodeErrorCode(t, rec.Body.Bytes()))
	}
}

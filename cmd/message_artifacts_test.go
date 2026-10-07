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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cliArtifactA = "5f1c2d3e-0000-4000-8000-0000000000aa"
	cliArtifactB = "5f1c2d3e-0000-4000-8000-0000000000bb"
)

func TestParseArtifactFlags(t *testing.T) {
	refs, err := parseArtifactFlags([]string{"scion://artifact/" + cliArtifactA + "@2", strings.ToUpper(cliArtifactB)})
	require.NoError(t, err)
	assert.Equal(t, []artifacts.MessageRef{{ArtifactID: cliArtifactA, Seq: 2}, {ArtifactID: cliArtifactB}}, refs)

	for _, bad := range [][]string{
		{"not-a-ref"},
		{"scion://artifact/" + cliArtifactA + "@0"},
		{cliArtifactA, "scion://artifact/" + cliArtifactA + "@3"}, // same artifact twice
	} {
		_, err := parseArtifactFlags(bad)
		assert.Error(t, err, "%v", bad)
	}
	tooMany := make([]string, artifacts.MaxMessageRefs+1)
	for i := range tooMany {
		tooMany[i] = cliArtifactA
	}
	_, err = parseArtifactFlags(tooMany)
	assert.ErrorContains(t, err, "too many --artifact")

	refs, err = parseArtifactFlags(nil)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestAppendArtifactRefsToBody(t *testing.T) {
	a := artifacts.MessageRef{ArtifactID: cliArtifactA}
	b := artifacts.MessageRef{ArtifactID: cliArtifactB, Seq: 1}
	assert.Equal(t, "hi\n\n"+a.String()+"\n"+b.String(), appendArtifactRefsToBody("hi", []artifacts.MessageRef{a, b}))
	assert.Equal(t, "see "+a.String()+"\n\n"+b.String(), appendArtifactRefsToBody("see "+a.String(), []artifacts.MessageRef{a, b}),
		"a reference already in the body is not repeated")
	assert.Equal(t, a.String(), appendArtifactRefsToBody("", []artifacts.MessageRef{a}))
	assert.Equal(t, "hi", appendArtifactRefsToBody("hi", nil))
}

func TestArtifactFlagValidation(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	defer resetMessageFlags()()

	ref := "scion://artifact/" + cliArtifactA
	cases := []struct {
		name   string
		setup  func()
		args   []string
		errMsg string
	}{
		{"with --in", func() { msgArtifacts = []string{ref}; msgIn = "5m" }, []string{"agent1", "hi"}, "--artifact cannot be combined with --in or --at"},
		{"with group", func() { msgArtifacts = []string{ref} }, []string{"group[a,b]", "hi"}, "--artifact cannot be used with group[] recipients"},
		{"malformed", func() { msgArtifacts = []string{"nope"} }, []string{"agent1", "hi"}, "--artifact \"nope\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := resetMessageFlags()
			defer restore()
			tc.setup()
			err := messageCmd.RunE(messageCmd, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errMsg)
		})
	}
}

// TestSendMessageViaHub_ArtifactRefs: the structured message carries the
// references in metadata (the hub decides what to keep).
func TestSendMessageViaHub_ArtifactRefs(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	defer resetMessageFlags()()

	projectID := "project-msg-artifact"
	server, sent := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	msgArtifactRefs = []artifacts.MessageRef{{ArtifactID: cliArtifactA, Seq: 2}}
	body := appendArtifactRefsToBody("design ready", msgArtifactRefs)
	require.NoError(t, sendMessageViaHub(hubCtx, "my-agent", body, false, false, false))

	require.Len(t, *sent, 1)
	sm := (*sent)[0].StructuredMsg
	require.NotNil(t, sm)
	assert.Equal(t, "design ready\n\nscion://artifact/"+cliArtifactA+"@2", sm.Msg)
	assert.Equal(t, `["scion://artifact/`+cliArtifactA+`@2"]`, sm.Metadata[artifacts.MessageMetadataKey])
}

//go:build !no_sqlite

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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// BenchmarkUserMeInboxSummary_50Projects measures GET /api/v1/users/me/inbox/summary
// latency on a workstation SQLite hub with 50 member projects and all five
// categories populated (design §4.5 target: p95 < 150 ms on SQLite).
func BenchmarkUserMeInboxSummary_50Projects(b *testing.B) {
	srv, s := testServer(b)
	artStore, _ := enableArtifactsForTest(b, srv)
	setExperimentOverrides(b, srv, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.Decisions:      true,
		experiments.Artifacts:      true,
	})
	wcs := srv.webChatStore
	require.NotNil(b, wcs)

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	me := &store.User{
		ID:          api.NewUUID(),
		Email:       "bench-me@example.com",
		DisplayName: "Bench User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     now,
	}
	peer := &store.User{
		ID:          api.NewUUID(),
		Email:       "bench-peer@example.com",
		DisplayName: "Bench Peer",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     now,
	}
	require.NoError(b, s.CreateUser(ctx, me))
	require.NoError(b, s.CreateUser(ctx, peer))

	const numProjects = 50
	projects := make([]*store.Project, numProjects)
	for i := 0; i < numProjects; i++ {
		p := &store.Project{
			ID:      api.NewUUID(),
			Slug:    fmt.Sprintf("bench-proj-%02d", i),
			Name:    fmt.Sprintf("Bench Project %02d", i),
			OwnerID: me.ID,
			Created: now.Add(-time.Duration(numProjects-i) * time.Minute),
			Updated: now.Add(-time.Duration(numProjects-i) * time.Minute),
		}
		require.NoError(b, s.CreateProject(ctx, p))
		require.NoError(b, s.CreateRoleBinding(ctx, &store.RoleBinding{
			ID:            api.NewUUID(),
			PrincipalType: store.PrincipalTypeUser,
			PrincipalID:   me.ID,
			RoleID:        store.ProjectRoleMember,
			ScopeType:     store.ScopeTypeProject,
			ScopeID:       p.ID,
			Created:       now,
		}))
		projects[i] = p

		// Create a general topic in every project so chat unread walks all 50 projects.
		topicID := api.NewUUID()
		require.NoError(b, wcs.CreateTopic(ctx, WebChatTopic{
			ID:             topicID,
			ProjectID:      p.ID,
			Name:           "general",
			IsGeneral:      true,
			CreatedBy:      me.ID,
			CreatedAt:      now.Add(-time.Hour),
			LastActivityAt: now.Add(-time.Duration(i+1) * time.Minute),
		}))

		// Populate unread chat messages in 10 projects (5 with @mention of me).
		if i < 10 {
			msgID := api.NewUUID()
			require.NoError(b, s.CreateMessage(ctx, &store.Message{
				ID:        msgID,
				ProjectID: p.ID,
				ThreadID:  topicID,
				Sender:    "user:" + peer.ID,
				SenderID:  peer.ID,
				Recipient: "project:" + p.ID,
				Msg:       fmt.Sprintf("Unread update %d", i),
				Type:      messages.TypeInstruction,
				CreatedAt: now.Add(-time.Duration(i+1) * time.Minute),
			}))
			require.NoError(b, wcs.TouchTopicActivity(ctx, topicID, msgID))
			if i < 5 {
				require.NoError(b, wcs.RecordMentions(ctx, topicID, msgID, []string{me.ID}))
			}
		}

		// Populate waiting agents and open decisions in 5 projects.
		if i < 5 {
			ag := &store.Agent{
				ID:          api.NewUUID(),
				Slug:        fmt.Sprintf("bench-agent-%02d", i),
				Name:        fmt.Sprintf("Bench Agent %02d", i),
				ProjectID:   p.ID,
				OwnerID:     me.ID,
				Ancestry:    []string{me.ID},
				MessageMode: store.MessageModeProject,
				Phase:       string(state.PhaseRunning),
				Activity:    string(state.ActivityWaitingForInput),
				Message:     "Need approval to continue",
			}
			require.NoError(b, s.CreateAgent(ctx, ag))

			decs, err := messages.NormalizeDecisions("decisions", []messages.DecisionInput{{
				ID:       fmt.Sprintf("D%d", i+1),
				Question: fmt.Sprintf("Proceed with step %d?", i+1),
				Choices: []messages.ChoiceInput{
					{ID: "yes", Label: "Yes"},
					{ID: "no", Label: "No"},
				},
			}})
			require.NoError(b, err)
			_, err = s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
				Message: &store.Message{
					ID:        api.NewUUID(),
					ProjectID: p.ID,
					Sender:    "agent:" + ag.Slug,
					SenderID:  ag.ID,
					Recipient: "chat",
					ThreadID:  topicID,
					Channel:   "web",
					CreatedAt: now.Add(-time.Duration(10-i) * time.Minute),
				},
				Intro:          "Please decide",
				AskerAgentID:   ag.ID,
				AskerAgentName: ag.Slug,
				Decisions:      decs,
			})
			require.NoError(b, err)
		}

		// Populate pending user-scope secrets (approvals) in 3 projects.
		if i < 3 {
			_, err := s.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
				ID:          api.NewUUID(),
				Key:         fmt.Sprintf("BENCH_SECRET_%d", i),
				SecretType:  store.SecretTypeEnvironment,
				Scope:       store.ScopeUser,
				ScopeID:     me.ID,
				ValueRef:    "bench-ref",
				ValueLen:    8,
				AgentID:     api.NewUUID(),
				AgentSlug:   fmt.Sprintf("bench-agent-%02d", i),
				ProjectID:   p.ID,
				ProjectName: p.Name,
				Created:     now,
				Updated:     now,
				ExpiresAt:   now.Add(time.Duration(i+1) * time.Hour),
			}, now)
			require.NoError(b, err)
		}

		// Populate artifact reviews (toAddress + requested) in 4 projects.
		if i < 4 {
			artID := uuid.NewString()
			ownerRef := me.ID
			kind := artifacts.VersionKindReview
			if i >= 2 {
				ownerRef = peer.ID
				kind = artifacts.VersionKindPublish
			}
			a := &artifacts.Artifact{
				ID: artID, ScopeKind: artifacts.ScopeKindProject, ScopeRef: p.ID,
				OwnerKind: artifacts.PrincipalKindUser, OwnerRef: ownerRef,
				Title: fmt.Sprintf("Bench Artifact %d", i), CurrentSeq: 1,
				CreatedAt: now, UpdatedAt: now,
			}
			v := &artifacts.Version{
				ID: uuid.NewString(), ArtifactID: artID, Seq: 1, Kind: kind,
				EntryPath: "doc.md", TotalBytes: 4, FileCount: 1,
				CreatedByKind: artifacts.PrincipalKindUser, CreatedByRef: ownerRef,
				CreatedAt: now, State: artifacts.VersionStateReady,
			}
			grants := []artifacts.Grant{{
				ID: uuid.NewString(), ArtifactID: artID, SubjectKind: artifacts.SubjectScope,
				SubjectRef: p.ID, Permission: artifacts.GrantRead,
				CreatedByRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, ownerRef), CreatedAt: now,
			}}
			require.NoError(b, artStore.CreatePublished(ctx, a, v, nil, grants))
			if i >= 2 {
				require.NoError(b, s.CreateNotification(ctx, &store.Notification{
					ID:             api.NewUUID(),
					SubscriptionID: uuid.Nil.String(),
					AgentID:        uuid.Nil.String(),
					ProjectID:      p.ID,
					SubscriberType: store.SubscriberTypeUser,
					SubscriberID:   me.ID,
					Kind:           store.NotificationKindArtifactGrant,
					ResourceType:   store.NotificationResourceArtifact,
					ResourceID:     artID,
					Status:         store.NotificationStatusArtifactReviewRequested,
					Message:        "Bench Peer asked you to review an artifact (Can review): " + artifacts.FormatRef(artID, 0),
					CreatedAt:      now,
				}))
			}
		}
	}

	// Seed 1 unread user DM.
	dmKey, err := messages.DMConversationKey("user", me.ID, "user", peer.ID)
	require.NoError(b, err)
	dmConvID := api.NewUUID()
	require.NoError(b, s.CreateConversation(ctx, &store.Conversation{
		ID:        dmConvID,
		Channel:   "native",
		ThreadKey: dmKey,
		Kind:      "direct",
		CreatedAt: now,
	}))
	dmMsgID := api.NewUUID()
	require.NoError(b, wcs.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   me.ID,
		PeerID:          peer.ID,
		PeerKind:        "user",
		LastMessageID:   dmMsgID,
		LastActivityAt:  now,
	}))
	require.NoError(b, s.CreateMessage(ctx, &store.Message{
		ID:             dmMsgID,
		ProjectID:      projects[0].ID,
		ThreadID:       dmKey,
		ConversationID: dmConvID,
		Channel:        "web",
		Sender:         "user:" + peer.ID,
		SenderID:       peer.ID,
		Recipient:      "user:" + me.ID,
		RecipientID:    me.ID,
		Msg:            "hello dm",
		Type:           messages.TypeInstruction,
		CreatedAt:      now,
	}))

	authUser := NewAuthenticatedUser(me.ID, me.Email, me.DisplayName, me.Role, string(ClientTypeWeb))
	handler := srv.Handler()

	// Sanity-check one warm-up request before timing.
	warmReq := httptest.NewRequest(http.MethodGet, userMeInboxSummaryPath, nil)
	warmReq = warmReq.WithContext(contextWithIdentity(warmReq.Context(), authUser))
	warmRec := httptest.NewRecorder()
	handler.ServeHTTP(warmRec, warmReq)
	require.Equal(b, http.StatusOK, warmRec.Code, warmRec.Body.String())

	durations := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, userMeInboxSummaryPath, nil)
		req = req.WithContext(contextWithIdentity(req.Context(), authUser))
		rec := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(rec, req)
		elapsed := time.Since(start)
		if rec.Code != http.StatusOK {
			b.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
		}
		durations = append(durations, elapsed)
	}
	b.StopTimer()

	if len(durations) > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p50 := durations[(len(durations)-1)*50/100]
		p95 := durations[(len(durations)-1)*95/100]
		b.ReportMetric(float64(p50)/float64(time.Millisecond), "p50_ms")
		b.ReportMetric(float64(p95)/float64(time.Millisecond), "p95_ms")
	}
}

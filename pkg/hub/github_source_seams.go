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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	triggithub "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// githubSourceWireVerifier returns GitHub's source.WireVerifier backed by the
// Server's configured AppID and clock.
func (s *Server) githubSourceWireVerifier() source.WireVerifier {
	return &triggithub.WireVerifier{
		ExpectedAppID: s.githubAppConfiguredID(),
		Now:           s.githubWebhookIngress.now,
	}
}

// githubSourceInstallationResolver returns GitHub's source.InstallationResolver
// backed by Server's installation store lookup, candidate/primary routing, and
// installation/installation_repositories lifecycle handlers.
func (s *Server) githubSourceInstallationResolver() source.InstallationResolver {
	return &triggithub.InstallationResolver{
		LookupInstallation: func(ctx context.Context, installationID int64) (source.Installation, error) {
			if s.store == nil {
				return source.Installation{}, errors.New("trigger store is not available")
			}
			inst, err := s.store.GetGitHubInstallation(ctx, installationID)
			if err != nil {
				return source.Installation{}, err
			}
			var routing map[string]string
			if len(inst.TriggerRouting) > 0 {
				routing = make(map[string]string, len(inst.TriggerRouting))
				for repo, route := range inst.TriggerRouting {
					if pid := strings.TrimSpace(route.PrimaryProjectID); pid != "" {
						routing[strings.ToLower(strings.TrimSpace(repo))] = pid
					}
				}
			}
			return source.Installation{
				ID:      strconv.FormatInt(inst.InstallationID, 10),
				Account: inst.AccountLogin,
				Active:  inst.Status == store.GitHubInstallationStatusActive,
				Routing: routing,
			}, nil
		},
		ResolveTargets: func(ctx context.Context, inst source.Installation, repoFullName string) ([]source.Target, error) {
			if s.store == nil {
				return nil, errors.New("trigger store is not available")
			}
			instID, err := strconv.ParseInt(strings.TrimSpace(inst.ID), 10, 64)
			if err != nil || instID <= 0 {
				return nil, fmt.Errorf("invalid github installation id %q", inst.ID)
			}
			candidates, err := s.listGitHubInstallationCandidateProjects(ctx, instID, repoFullName)
			if err != nil {
				return nil, err
			}
			if len(candidates) == 0 {
				return nil, nil
			}
			routing := make(map[string]store.GitHubTriggerRoute, len(inst.Routing))
			for repo, primaryID := range inst.Routing {
				routing[repo] = store.GitHubTriggerRoute{PrimaryProjectID: primaryID}
			}
			selected, bypassed, routingStatus := ApplyPrimaryRouting(candidates, routing, repoFullName)
			targets := make([]source.Target, len(selected))
			for i := range selected {
				targets[i] = source.Target{
					ProjectID:          selected[i].ID,
					InstallationID:     inst.ID,
					Resource:           repoFullName,
					RoutingStatus:      routingStatus,
					BypassedProjectIDs: append([]string(nil), bypassed...),
				}
			}
			return targets, nil
		},
		HandleLifecycle: func(ctx context.Context, d source.Delivery, body []byte) (bool, error) {
			switch d.Event {
			case triggithub.EventInstallation:
				return true, s.processInstallationWebhook(ctx, body)
			case triggithub.EventInstallationRepositories:
				return true, s.processInstallationRepositoriesWebhook(ctx, body)
			default:
				return false, nil
			}
		},
	}
}

// githubSourceNormalizer returns GitHub's source.Normalizer wired to Server's
// App mention identity cache.
func (s *Server) githubSourceNormalizer() source.Normalizer {
	return &triggithub.EventNormalizer{
		OptionsFn: func(ctx context.Context, d source.Delivery) triggithub.Options {
			slug, aliases, _ := s.GitHubAppMentionIdentities(ctx)
			return triggithub.Options{
				AppSlug:        slug,
				MentionAliases: aliases,
				ReceivedAt:     d.ReceivedAt,
			}
		},
	}
}

// githubSourceActorIdentity returns GitHub's source.ActorIdentity backed by
// Server's App bot login and configured AppID.
func (s *Server) githubSourceActorIdentity() source.ActorIdentity {
	return &triggithub.AppActorIdentity{
		Resolve: func(ctx context.Context) (string, int64) {
			return s.GitHubAppBotLogin(ctx), s.githubAppConfiguredID()
		},
	}
}

// githubSourceIdentityMinter returns GitHub's source.IdentityMinter backed by
// Server.MintGitHubAppTokenWithPermissions (with association authorization,
// project permission limits, and in-memory token caching).
func (s *Server) githubSourceIdentityMinter() source.IdentityMinter {
	return &triggithub.TokenMinter{
		MintFunc: func(ctx context.Context, t source.Target, scope source.CredentialScope) (source.Credential, error) {
			if s.store == nil {
				return source.Credential{}, errors.New("trigger store is not available")
			}
			project, err := s.store.GetProject(ctx, t.ProjectID)
			if err != nil {
				return source.Credential{}, err
			}
			perms, err := githubapp.TokenPermissionsFromScope(scope)
			if err != nil {
				return source.Credential{}, err
			}
			token, expStr, err := s.MintGitHubAppTokenWithPermissions(ctx, project, perms)
			if err != nil {
				return source.Credential{}, err
			}
			exp, _ := time.Parse(time.RFC3339, expStr)
			granted := make(map[string]string, len(scope.Permissions))
			for k, v := range scope.Permissions {
				granted[k] = v
			}
			return source.Credential{
				Token:     token,
				ExpiresAt: exp,
				Scope:     granted,
			}, nil
		},
	}
}

// githubSourceCommentAction returns GitHub's source.ProviderAction for
// github.comment backed by githubCommentAction's validation, per-project rate
// bucket (AllowGitHubComment), IdentityMinter credential minting, and
// idempotent comment posting (PostGitHubCommentIdempotent).
func (s *Server) githubSourceCommentAction() source.ProviderAction {
	commentAction := &githubCommentAction{srv: s}
	minter := s.githubSourceIdentityMinter()
	return &triggithub.CommentAction{
		DefaultMinter: minter,
		ValidateFunc: func(ctx context.Context, req source.ActionValidateRequest) (string, error) {
			return commentAction.Validate(ctx, nil, req.ProjectID, req.Payload)
		},
		AllowRate: s.AllowGitHubComment,
		FireFunc: func(ctx context.Context, req source.ActionFireRequest) (source.ActionFireResult, error) {
			payload, tmpl, err := parseAndValidateGitHubCommentPayload(req.Payload)
			if err != nil {
				return source.ActionFireResult{Status: string(hubfire.FireStatusFailed), Error: "invalid github.comment payload"}, nil
			}
			target, err := extractGitHubCommentTarget(req.Event.Type, req.Event.Attributes, payload.ReplyInThread)
			if err != nil {
				return source.ActionFireResult{Status: string(hubfire.FireStatusFailed), Error: "github comment failed"}, nil
			}
			project, err := s.store.GetProject(ctx, req.ProjectID)
			if err != nil {
				return source.ActionFireResult{}, err
			}
			vars := buildGitHubCommentTemplateVars(&hubfire.FireEvent{
				ID:         req.Event.DeliveryID,
				SourceKind: req.Event.SourceKind,
				SourceID:   req.Event.SourceID,
				Type:       req.Event.Type,
				Subject:    req.Event.Subject,
				OccurredAt: req.Event.OccurredAt,
				Attributes: req.Event.Attributes,
				Lists:      req.Event.Lists,
			}, project)
			renderedBody, _, err := tmpl.Render(vars, payload.UntrustedVars)
			if err != nil {
				return source.ActionFireResult{Status: string(hubfire.FireStatusFailed), Error: "github comment failed"}, nil
			}

			mintTarget := req.Target
			if mintTarget.ProjectID == "" {
				mintTarget.ProjectID = req.ProjectID
			}
			cred, err := req.Minter.Mint(ctx, mintTarget, source.CredentialScope{
				Permissions: map[string]string{"issues": "write", "metadata": "read"},
			})
			if err != nil {
				return source.ActionFireResult{Status: string(hubfire.FireStatusRefused), Error: "github comment refused"}, err
			}

			attempts := req.Attempts
			if attempts <= 0 {
				attempts = 1
			}
			comment, err := s.PostGitHubCommentIdempotent(ctx, cred.Token, githubapp.PostCommentOptions{
				Owner:           target.owner,
				Repo:            target.repo,
				IssueNumber:     target.number,
				ReviewCommentID: target.reviewCommentID,
				Body:            renderedBody,
				FireID:          req.FireID,
				Attempts:        attempts,
			})
			if err != nil {
				return source.ActionFireResult{Status: string(hubfire.FireStatusFailed), Error: "github comment failed"}, err
			}
			return source.ActionFireResult{
				Status:    string(hubfire.FireStatusFired),
				ResultRef: strconv.FormatInt(comment.ID, 10),
			}, nil
		},
	}
}

// githubSourceReadiness returns GitHub's source.Readiness backed by Server's
// App identity cache and webhook configuration.
func (s *Server) githubSourceReadiness() source.Readiness {
	return &triggithub.ReadinessChecker{
		Snapshot: func(ctx context.Context) (triggithub.ReadinessSnapshot, error) {
			cfg, _, hasWebhookSecret := s.githubAppCredentialPresence(ctx)
			full := s.computeGitHubTriggersReadiness(ctx, cfg, hasWebhookSecret)
			id, _, _ := s.ensureGitHubAppIdentity(ctx)
			snap := triggithub.ReadinessSnapshot{
				Configured:       cfg.AppID != 0 && id != nil,
				WebhooksEnabled:  full.WebhooksEnabled,
				HasWebhookSecret: full.HasWebhookSecret,
				AppSlug:          full.AppSlug,
				CheckedAt:        full.CheckedAt,
				Error:            full.Error,
			}
			if id != nil {
				snap.Events = append([]string(nil), id.Events...)
				snap.Permissions = make(map[string]string, len(id.Permissions))
				for k, v := range id.Permissions {
					snap.Permissions[k] = v
				}
			}
			return snap, nil
		},
	}
}

// githubSourcePresets returns GitHub's source.Presets catalog.
func (s *Server) githubSourcePresets() source.Presets {
	return &triggithub.PresetCatalog{}
}

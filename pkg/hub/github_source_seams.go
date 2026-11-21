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
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	triggithub "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GitHubSourceWireVerifier returns GitHub's source.WireVerifier backed by the
// Server's configured AppID and clock.
func (s *Server) GitHubSourceWireVerifier() source.WireVerifier {
	return &triggithub.WireVerifier{
		ExpectedAppID: s.githubAppConfiguredID(),
		Now:           s.githubWebhookIngress.now,
	}
}

// GitHubSourceInstallationResolver returns GitHub's source.InstallationResolver
// backed by Server's installation store lookup, candidate/primary routing, and
// installation/installation_repositories lifecycle handlers.
func (s *Server) GitHubSourceInstallationResolver() source.InstallationResolver {
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

// GitHubSourceNormalizer returns GitHub's source.Normalizer wired to Server's
// in-memory App mention identity cache (pure, no DB or network I/O, matching
// webhook ingress step 4).
func (s *Server) GitHubSourceNormalizer() source.Normalizer {
	return &triggithub.EventNormalizer{
		OptionsFn: func(_ context.Context, d source.Delivery) triggithub.Options {
			slug, aliases, _ := s.peekGitHubAppMentionIdentities()
			return triggithub.Options{
				AppSlug:        slug,
				MentionAliases: aliases,
				ReceivedAt:     d.ReceivedAt,
			}
		},
	}
}

// GitHubSourceActorIdentity returns GitHub's source.ActorIdentity backed by
// Server's App bot login and configured AppID.
func (s *Server) GitHubSourceActorIdentity() source.ActorIdentity {
	return &triggithub.AppActorIdentity{
		Resolve: func(ctx context.Context) (string, int64) {
			return s.GitHubAppBotLogin(ctx), s.githubAppConfiguredID()
		},
	}
}

// GitHubSourceIdentityMinter returns GitHub's source.IdentityMinter backed by
// Server.MintGitHubAppTokenWithPermissions (with association authorization,
// project permission limits, and in-memory token caching).
func (s *Server) GitHubSourceIdentityMinter() source.IdentityMinter {
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
			token, expStr, granted, err := s.mintGitHubAppTokenInternal(ctx, project, &perms, true)
			if err != nil {
				return source.Credential{}, err
			}
			exp, err := time.Parse(time.RFC3339, expStr)
			if err != nil {
				return source.Credential{}, fmt.Errorf("parse github installation token expiry: %w", err)
			}
			return source.Credential{
				Token:     token,
				ExpiresAt: exp,
				Scope:     granted,
			}, nil
		},
	}
}

// GitHubSourceCommentAction returns GitHub's source.ProviderAction for
// github.comment delegating directly to the registered github.comment
// FireAction (Validate, Preauthorize, Authorize, Fire, rate bucket, token
// minting, marker-idempotent posting with Since, outcome classification, and
// audit).
func (s *Server) GitHubSourceCommentAction() source.ProviderAction {
	commentAction := &githubCommentAction{srv: s}
	return &triggithub.CommentAction{
		ValidateFunc: func(ctx context.Context, req source.ActionValidateRequest) (string, error) {
			return commentAction.Validate(ctx, nil, req.ProjectID, req.Payload)
		},
		FireFunc: func(ctx context.Context, req source.ActionFireRequest) (source.ActionFireResult, error) {
			if s.store == nil {
				return source.ActionFireResult{
					Status: string(hubfire.FireStatusFailed),
					Error:  "github comment failed",
				}, nil
			}
			if strings.TrimSpace(req.TriggerID) == "" {
				return source.ActionFireResult{
					Status: string(hubfire.FireStatusRefused),
					Error:  "github.comment refused",
				}, nil
			}
			tr, err := s.store.GetTrigger(ctx, req.TriggerID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
					return source.ActionFireResult{
						Status: string(hubfire.FireStatusRefused),
						Error:  "github.comment refused",
					}, nil
				}
				return source.ActionFireResult{
					Status: string(hubfire.FireStatusFailed),
					Error:  "github comment failed",
				}, nil
			}
			if tr.ProjectID != req.ProjectID {
				return source.ActionFireResult{
					Status: string(hubfire.FireStatusRefused),
					Error:  "github.comment refused",
				}, nil
			}

			var lists map[string][]string
			if len(req.Event.Lists) > 0 {
				lists = make(map[string][]string, len(req.Event.Lists))
				for k, vals := range req.Event.Lists {
					lists[k] = append([]string(nil), vals...)
				}
			}

			fireReq := hubfire.FireRequest{
				ProjectID: tr.ProjectID,
				Authority: hubfire.RecordedAuthority{
					Attribution:    tr.InitiatorAttribution,
					Ceiling:        tr.AuthorityCeiling,
					DefinitionKind: hubfire.DefinitionKindTrigger,
					DefinitionID:   tr.ID,
					FireID:         req.FireID,
				},
				ActionType: store.TriggerActionGitHubComment,
				Payload:    req.Payload,
				Event: &hubfire.FireEvent{
					ID:         req.Event.ID,
					ProjectID:  tr.ProjectID,
					SourceKind: req.Event.SourceKind,
					SourceID:   req.Event.SourceID,
					Type:       req.Event.Type,
					Subject:    req.Event.Subject,
					OccurredAt: req.Event.OccurredAt,
					ReceivedAt: req.Event.ReceivedAt,
					Attributes: maps.Clone(req.Event.Attributes),
					Lists:      lists,
					Payload:    req.Event.Payload,
					Trigger: &hubfire.FireTriggerRef{
						ID:   tr.ID,
						Name: tr.Name,
					},
				},
				FireAt:   req.Event.ReceivedAt,
				Attempts: req.Attempts,
			}

			out := s.fireAction(ctx, fireReq)
			return source.ActionFireResult{
				Status:    out.Status.String(),
				ResultRef: out.ResultRef,
				Error:     out.Error,
			}, nil
		},
	}
}

// GitHubSourceReadiness returns GitHub's source.Readiness backed directly by
// Server's canonical computeGitHubTriggersReadiness evaluator (single table,
// single evaluator, 1:1 field projection into source.Report).
func (s *Server) GitHubSourceReadiness() source.Readiness {
	return &triggithub.ReadinessChecker{
		CheckFunc: func(ctx context.Context) (source.Report, error) {
			cfg, _, hasWebhookSecret := s.githubAppCredentialPresence(ctx)
			full := s.computeGitHubTriggersReadiness(ctx, cfg, hasWebhookSecret)
			return source.Report{
				Ready:              full.Ready,
				WebhooksEnabled:    full.WebhooksEnabled,
				HasWebhookSecret:   full.HasWebhookSecret,
				MissingEvents:      append([]string{}, full.MissingEvents...),
				MissingPermissions: append([]string{}, full.MissingPermissions...),
				AppSlug:            full.AppSlug,
				CheckedAt:          full.CheckedAt,
				Error:              full.Error,
			}, nil
		},
	}
}

// GitHubSourcePresets returns GitHub's source.Presets catalog.
func (s *Server) GitHubSourcePresets() source.Presets {
	return &triggithub.PresetCatalog{}
}

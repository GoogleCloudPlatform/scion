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

package hubprofile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// KindTestFixture is the kind of every identity issued through a profile.
const KindTestFixture = "test_fixture"

// runPurposePrefix marks an identity's purpose with its run and alias, so
// teardown can find an identity whose cache file was never written.
const runPurposePrefix = "scion-run:"

// RunPurpose returns the purpose recorded on an identity issued for alias
// in run, followed by the caller's own purpose when given.
func RunPurpose(run, alias, purpose string) string {
	tag := runPurposePrefix + run + "/" + alias
	if purpose = strings.TrimSpace(purpose); purpose != "" {
		return tag + " " + purpose
	}
	return tag
}

// purposeRun returns the run label of a purpose written by RunPurpose.
func purposeRun(purpose string) (string, bool) {
	rest, ok := strings.CutPrefix(purpose, runPurposePrefix)
	if !ok {
		return "", false
	}
	run, _, ok := strings.Cut(rest, "/")
	return run, ok
}

// IssueRequest are the inputs of IssueToCache.
type IssueRequest struct {
	Alias    string
	Role     string
	Purpose  string
	Lifetime int64
	TokenTTL int64
}

// IssueToCache issues a member or viewer test identity through the
// profile's member issuer delegation and stores it in the run cache as
// req.Alias. The cache file is reserved exclusively first, so a taken
// alias or a refused path costs no identity. The returned entry holds the
// token; callers must not print it.
func IssueToCache(ctx context.Context, s *Session, req IssueRequest) (*CacheEntry, error) {
	if s.opts.Run == "" {
		return nil, errors.New("--run <label> is required: issued identities are cached per run")
	}
	if err := ValidateName("alias", req.Alias); err != nil {
		return nil, err
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "member"
	}
	if adminTiers[role] || role == "hub-admin" {
		return nil, fmt.Errorf("role %q is not supported through a hub profile: profiles issue member and viewer identities only", role)
	}
	if role != "member" && role != "viewer" {
		return nil, fmt.Errorf(`invalid --role %q: must be "member" or "viewer"`, req.Role)
	}
	client, err := s.IssuerClient()
	if err != nil {
		return nil, err
	}
	res, err := ReserveEntry(s.opts.Home, s.opts.Profile.Name, s.opts.Run, req.Alias)
	if err != nil {
		return nil, err
	}
	resp, err := client.TestIdentities().Issue(ctx, &hubclient.IssueTestIdentityRequest{
		Role: role, Purpose: RunPurpose(s.opts.Run, req.Alias, req.Purpose),
		LifetimeSeconds: req.Lifetime, TokenTTLSeconds: req.TokenTTL,
	})
	if err != nil {
		res.Abandon()
		return nil, fmt.Errorf("failed to issue test identity: %w", err)
	}
	e := &CacheEntry{
		Alias: req.Alias, Run: s.opts.Run, Profile: s.opts.Profile.Name, Endpoint: s.Endpoint(),
		IdentityID: resp.Identity.ID, Email: resp.Identity.Email, Kind: KindTestFixture, Role: resp.Identity.Role,
		DelegationID: s.opts.Profile.MemberDelegation(),
		ExpiresAt:    resp.Identity.ExpiresAt, TokenExpiresAt: resp.TokenExpiresAt, Token: resp.AccessToken,
	}
	if err := res.Fill(e); err != nil {
		_ = RemoveEntry(s.opts.Home, s.opts.Profile.Name, s.opts.Run, req.Alias)
		// Do not leave an identity nobody can use.
		if derr := client.TestIdentities().Delete(ctx, resp.Identity.ID); derr != nil && !apiclient.IsNotFoundError(derr) {
			return nil, fmt.Errorf("test identity %s was issued but could not be cached (%v), and deleting it failed (%v); 'scion hub test-identity teardown --run %s' removes it", resp.Identity.ID, err, derr, s.opts.Run)
		}
		return nil, fmt.Errorf("test identity was issued but could not be cached, so it was deleted: %w", err)
	}
	return e, nil
}

// TeardownItem is the outcome for one identity.
type TeardownItem struct {
	Alias      string `json:"alias,omitempty"`
	IdentityID string `json:"identityId"`
	Outcome    string `json:"outcome"` // deleted, purged, already-gone, failed
	Detail     string `json:"detail,omitempty"`
}

// TeardownReport is the result of Teardown.
type TeardownReport struct {
	Profile      string         `json:"profile"`
	Run          string         `json:"run"`
	Items        []TeardownItem `json:"items"`
	CacheRemoved bool           `json:"cacheRemoved"`
	Complete     bool           `json:"complete"`
}

// Teardown deletes every identity of the session's run through the
// profile's issuer delegation: each cached alias, and any identity the
// hub lists for the delegation whose purpose names this run (one whose
// cache file was never written). Each alias leaves the cache once its
// identity is gone; the run directory is removed when all are gone.
func Teardown(ctx context.Context, s *Session, purge bool) (*TeardownReport, error) {
	if s.opts.Run == "" {
		return nil, errors.New("--run <label> is required")
	}
	home, profile, run := s.opts.Home, s.opts.Profile.Name, s.opts.Run
	client, err := s.IssuerClient()
	if err != nil {
		return nil, err
	}
	rep := &TeardownReport{Profile: profile, Run: run, Complete: true}
	svc := client.TestIdentities()
	done := map[string]bool{}

	remove := func(alias, id string) {
		item := TeardownItem{Alias: alias, IdentityID: id}
		var err error
		if purge {
			var report *hubclient.TestIdentityPurgeReport
			report, err = svc.Purge(ctx, id)
			if err == nil && report != nil && !report.Complete {
				err = errors.New("purge incomplete; run teardown again")
			}
			item.Outcome = "purged"
		} else {
			err = svc.Delete(ctx, id)
			item.Outcome = "deleted"
		}
		switch {
		case err == nil:
		case apiclient.IsNotFoundError(err):
			item.Outcome = "already-gone"
		default:
			item.Outcome = "failed"
			item.Detail = err.Error()
			rep.Complete = false
		}
		if item.Outcome != "failed" {
			done[id] = true
			if alias != "" {
				if err := RemoveEntry(home, profile, run, alias); err != nil {
					item.Detail = "identity removed, but its cache file was not: " + err.Error()
					rep.Complete = false
				}
			}
		}
		rep.Items = append(rep.Items, item)
	}

	aliases, err := ListRun(home, profile, run)
	if err != nil {
		return nil, err
	}
	for _, a := range aliases {
		e, err := ReadEntry(home, profile, run, a)
		if err != nil {
			rep.Items = append(rep.Items, TeardownItem{Alias: a, Outcome: "failed", Detail: err.Error()})
			rep.Complete = false
			continue
		}
		remove(a, e.IdentityID)
	}

	list, err := svc.List(ctx, &hubclient.ListTestIdentitiesOptions{Limit: 100})
	if err != nil {
		rep.Complete = false
		rep.Items = append(rep.Items, TeardownItem{Outcome: "failed", Detail: "listing the delegation's identities: " + err.Error()})
	} else {
		for _, it := range list.Items {
			if done[it.ID] {
				// Deleted above but still listed: report it.
				rep.Complete = false
				rep.Items = append(rep.Items, TeardownItem{IdentityID: it.ID, Outcome: "failed", Detail: "still listed after delete"})
				continue
			}
			if r, ok := purposeRun(it.Purpose); ok && r == run {
				remove("", it.ID)
			}
		}
		if list.Truncated {
			rep.Complete = false
			rep.Items = append(rep.Items, TeardownItem{Outcome: "failed", Detail: "the delegation lists more identities than one page; run teardown again"})
		}
	}

	if rep.Complete {
		if err := RemoveRunDir(home, profile, run); err != nil {
			rep.Complete = false
			rep.Items = append(rep.Items, TeardownItem{Outcome: "failed", Detail: "removing the run cache: " + err.Error()})
		} else {
			rep.CacheRemoved = true
		}
	}
	return rep, nil
}

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
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Ask for review (ptone/scion#5145): the hub's artifacts.GrantNotifier.
//
// MayNotifyGrant is the whole send authorization, asked before the grant
// is written and before the notify limiter is charged; it writes nothing.
// NotifyGrant writes one notice to the grant's subject after the grant
// committed. The notice carries ids, enums and a sanitised label only:
// never the artifact's title or any request text (K14/K19).

var _ artifacts.GrantNotifier = (*artifactHost)(nil)

// noticeLabelMaxRunes caps a notice label (the label rule).
const noticeLabelMaxRunes = 80

// noticeLabelFallback is the label of a granter with no usable name or
// email to show.
const noticeLabelFallback = "a user"

// errGrantNoticeActor refuses a notice whose actor is not a user. The agent
// branch is ptone/scion#5548.
var errGrantNoticeActor = errors.New("hub: grant notices are sent for a user actor only")

// errGrantNoticeUnavailable is MayNotifyGrant's fault when the host has no
// store or authz service to decide with.
var errGrantNoticeUnavailable = errors.New("hub: grant notices are unavailable")

// MayNotifyGrant implements artifacts.GrantNotifier (v5 2.4, v8 O4, F6):
//  1. the caller of ctx is an interactive, unscoped user session
//     (interactiveUserSession, the gate scheduledSendCaller uses);
//  2. the subject is an active user that can still sign in
//     (namedUserSubject);
//  3. the caller may read the subject user (CheckAccess on the caller's own
//     identity), as a direct message to that user would need.
//
// Every refusal is (false, nil), which the service answers as
// not_permitted; a failed lookup is (false, err), answered as unavailable.
func (h *artifactHost) MayNotifyGrant(ctx context.Context, subjectUserID string) (bool, error) {
	granter, ok := interactiveUserSession(ctx)
	if !ok {
		return false, nil
	}
	if h.server == nil || h.server.store == nil || h.server.authzService == nil {
		return false, errGrantNoticeUnavailable
	}
	subject, _, err := h.server.namedUserSubject(ctx, subjectUserID)
	if err != nil {
		return false, err
	}
	if subject == nil {
		return false, nil
	}
	return h.server.authzService.CheckAccess(ctx, granter, userResource(subject), ActionRead).Allowed, nil
}

// NotifyGrant implements artifacts.GrantNotifier: it writes the grant
// notice of n to its subject (the row shape, the label and email rules) through the
// shared writer, and returns the write error. It refuses an actor other
// than a user and a permission it has no template for.
func (h *artifactHost) NotifyGrant(ctx context.Context, n artifacts.GrantNotice) error {
	if n.ActorKind != artifacts.PrincipalKindUser || n.ActorRef == "" {
		return errGrantNoticeActor
	}
	if h.server == nil {
		return errGrantNoticeUnavailable
	}
	status, ok := grantNoticeStatus(n.Permission)
	if !ok {
		return errUserNoticeInvalid
	}
	label := h.server.grantNoticeLabel(ctx, n.SubjectUserID, n.ActorRef)
	return h.server.writeUserNotification(ctx, userNotice{
		Kind:             store.NotificationKindArtifactGrant,
		ResourceType:     store.NotificationResourceArtifact,
		ResourceID:       n.ArtifactID,
		Status:           status,
		ProjectID:        n.HomeScope,
		SubscriberUserID: n.SubjectUserID,
		Message:          grantNoticeMessage(label, n.Permission, n.ArtifactID),
	})
}

// grantNoticeStatus is the notification status of a grant of permission.
func grantNoticeStatus(permission string) (string, bool) {
	switch permission {
	case artifacts.GrantReview:
		return store.NotificationStatusArtifactReviewRequested, true
	case artifacts.GrantRead, artifacts.GrantWrite, artifacts.GrantAdmin:
		return store.NotificationStatusArtifactShared, true
	}
	return "", false
}

// grantNoticeLevel is the level of permission as the web UI names it.
var grantNoticeLevel = map[string]string{
	artifacts.GrantRead:   "Can view",
	artifacts.GrantReview: "Can review",
	artifacts.GrantWrite:  "Can edit",
	artifacts.GrantAdmin:  "Can manage",
}

// grantNoticeMessage is the fixed message of a grant notice: the label, the
// level and the artifact's reference, and nothing else (K14/K19).
func grantNoticeMessage(label, permission, artifactID string) string {
	ref := artifacts.FormatRef(artifactID, 0)
	if permission == artifacts.GrantReview {
		return label + " asked you to review an artifact (" + grantNoticeLevel[permission] + "): " + ref
	}
	return label + " shared an artifact with you (" + grantNoticeLevel[permission] + "): " + ref
}

// grantNoticeLabel names the granter in a notice to the subject user (the
// label and email rules): the granter's sanitised display name, followed by
// its email in parentheses only when policy lets the subject read the
// granter's user record (CheckAccess with the subject's check-only
// identity, under maskAllAuthzMemo). Any lookup failure or denial leaves the
// email out, and so does an email that cleaning would change or that is
// longer than the cap (alone or with the name): an email is shown exactly
// as stored, whole, or not at all. With no usable name the label is
// the email when it may be shown and fits, else "a user". It never refuses
// the notice.
func (s *Server) grantNoticeLabel(ctx context.Context, subjectUserID, granterUserID string) string {
	if s.store == nil || granterUserID == "" {
		return noticeLabelFallback
	}
	granter, err := s.store.GetUser(ctx, granterUserID)
	if err != nil || granter == nil {
		return noticeLabelFallback
	}
	name := sanitizeNoticeLabel(granter.DisplayName)
	// The email is shown only as stored: cleaning must change nothing (a
	// stored control, format or bidi rune would make it display as another
	// address), and it is never capped, so it is shown whole or not at all.
	email := cleanNoticeText(granter.Email, -1)
	showEmail := false
	if email != "" && email == granter.Email && utf8.RuneCountInString(email) <= noticeLabelMaxRunes && s.authzService != nil {
		if _, subject, err := s.namedUserSubject(ctx, subjectUserID); err == nil && subject != nil {
			showEmail = s.authzService.CheckAccess(maskAllAuthzMemo(ctx), subject, userResource(granter), ActionRead).Allowed
		}
	}
	switch {
	case name != "" && showEmail && utf8.RuneCountInString(name)+len(" ()")+utf8.RuneCountInString(email) <= noticeLabelMaxRunes:
		return name + " (" + email + ")"
	case name != "":
		// Also a name and email too long together: an email is never
		// shown cut short.
		return name
	case showEmail:
		return email
	}
	return noticeLabelFallback
}

// sanitizeNoticeLabel makes s safe to show on one line (the label rule): it
// drops runes in Unicode categories Cc and Cf and the line and paragraph
// separators U+2028 and U+2029, collapses each run of white space to one
// space, trims, and caps the result at noticeLabelMaxRunes runes. Invalid
// UTF-8 is dropped. ptone/scion#7135 reuses it for its own labels.
func sanitizeNoticeLabel(s string) string {
	return cleanNoticeText(s, noticeLabelMaxRunes)
}

// cleanNoticeText is sanitizeNoticeLabel with a cap of limit runes, or no
// cap when limit is negative.
func cleanNoticeText(s string, limit int) string {
	var b strings.Builder
	n := 0         // runes written
	space := false // a white space run is pending
	for _, r := range s {
		switch {
		case r == utf8.RuneError, r == '\u2028', r == '\u2029',
			unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r):
			continue
		case unicode.IsSpace(r):
			space = n > 0
			continue
		}
		need := 1
		if space {
			need = 2
		}
		if limit >= 0 && n+need > limit {
			break
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
		n += need
	}
	return b.String()
}

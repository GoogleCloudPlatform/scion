/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * The sections of the artifact access dialog, decided from the capability
 * flags the hub returns on GET /api/v1/artifacts/{id} (ptone/scion#7107).
 * No DOM.
 *
 * The flags are advisory: every route the dialog calls decides again, so a
 * stale flag never authorizes anything. This module only chooses what to
 * show, and holds no authorization rule of its own beyond that choice.
 */

import type { ArtifactResponse } from '../../client/artifacts.js';

/** The capability flags the access dialog reads, from the artifact GET. */
export interface AccessCapabilities {
  canManage: boolean;
  canShare: boolean;
  canGrantReview: boolean;
  /** A steward's clause; '' for any caller who is not a steward. */
  shareAuthority: '' | 'project_owner' | 'delegator';
}

/**
 * People and access: "edit" is the full people section (search users and
 * projects, change and remove); "bounded" adds review access and removes
 * any grant (a steward); "review-only" only gives review access, with no
 * grant list; "remove-only" lists grants with Remove only.
 */
export type PeopleState = 'edit' | 'bounded' | 'review-only' | 'remove-only' | 'hidden';

/** Link sharing: "edit" creates and revokes; "revoke-only" lists and revokes. */
export type LinksState = 'edit' | 'revoke-only' | 'hidden';

/** Retention: "edit" changes the expiry; "read-only" shows it. */
export type RetentionState = 'edit' | 'read-only' | 'hidden';

export interface SectionStates {
  people: PeopleState;
  links: LinksState;
  retention: RetentionState;
}

export interface AccessOptions {
  /** Stewards may create links in the dialog (phase 2 of ptone/scion#7107). */
  stewardCreateLink: boolean;
}

/** Whether a steward is offered Create link. Off until phase 2. */
export const STEWARD_CREATE_LINK = false;

/** The capability flags of an artifact GET; absent flags are false. */
export function capabilitiesFrom(r: ArtifactResponse | null | undefined): AccessCapabilities {
  const auth = r?.shareAuthority;
  return {
    canManage: r?.canManage === true,
    canShare: r?.canShare === true,
    canGrantReview: r?.canGrantReview === true,
    shareAuthority: auth === 'project_owner' || auth === 'delegator' ? auth : '',
  };
}

/** Whether the page offers Manage access: the caller may change access or retention. */
export function showManageAccess(c: AccessCapabilities): boolean {
  return c.canShare || c.canGrantReview;
}

/** Each section's state for a caller with these capabilities. */
export function sectionStates(c: AccessCapabilities, o: AccessOptions): SectionStates {
  let people: PeopleState = 'hidden';
  if (c.canManage) people = 'edit';
  else if (c.canShare) people = 'bounded';
  else if (c.canGrantReview) people = 'review-only';

  let links: LinksState = 'hidden';
  if (c.canManage || (c.canShare && o.stewardCreateLink)) links = 'edit';
  else if (c.canShare) links = 'revoke-only';

  let retention: RetentionState = 'hidden';
  if (c.canShare) retention = 'edit';
  else if (c.canGrantReview) retention = 'read-only';

  return { people, links, retention };
}

/**
 * Whether the dialog loads the grants and links (both admin-only on the
 * hub): only when it shows a link list or a grant list.
 */
export function needsAccessData(st: SectionStates): boolean {
  return (
    st.links !== 'hidden' ||
    st.people === 'edit' ||
    st.people === 'bounded' ||
    st.people === 'remove-only'
  );
}

/** The section states of the dialog's fixed modes, as before Manage access. */
export const MODE_PRESETS: Readonly<Record<'share' | 'review' | 'retention', SectionStates>> = {
  share: { people: 'edit', links: 'edit', retention: 'edit' },
  review: { people: 'review-only', links: 'hidden', retention: 'hidden' },
  retention: { people: 'remove-only', links: 'revoke-only', retention: 'edit' },
};

/** Every section hidden: mode "manage" before its capabilities are known. */
export const NO_SECTIONS: Readonly<SectionStates> = {
  people: 'hidden',
  links: 'hidden',
  retention: 'hidden',
};

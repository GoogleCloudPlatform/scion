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
 * The section states of the Manage access dialog (ptone/scion#7107, design
 * section 4.1, phase 1 and phase 2 columns), from the capability flags of
 * the artifact GET.
 */

import { describe, expect, it } from 'vitest';

import type { ArtifactResponse } from '../../client/artifacts.js';
import {
  MODE_PRESETS,
  NO_SECTIONS,
  STEWARD_CREATE_LINK,
  capabilitiesFrom,
  needsAccessData,
  sectionStates,
  showManageAccess,
} from './artifact-access-sections.js';
import type {
  AccessCapabilities,
  LinksState,
  PeopleState,
  RetentionState,
  SectionStates,
} from './artifact-access-sections.js';

/** The expected states, written out from the matrix rather than from the code's branches. */
function expected(c: AccessCapabilities, stewardCreateLink: boolean): SectionStates {
  const manager = c.canManage;
  const steward = !c.canManage && c.canShare;
  const reviewOnly = !c.canManage && !c.canShare && c.canGrantReview;
  const people: PeopleState = manager
    ? 'edit'
    : steward
      ? 'bounded'
      : reviewOnly
        ? 'review-only'
        : 'hidden';
  const links: LinksState = manager
    ? 'edit'
    : steward
      ? stewardCreateLink
        ? 'edit'
        : 'revoke-only'
      : 'hidden';
  // Retention follows canShare alone (section 4.1): editable iff canShare,
  // read-only iff canGrantReview without canShare. The hub never sends
  // canManage without canShare, and such a caller gets no editable expiry.
  const retention: RetentionState = c.canShare ? 'edit' : c.canGrantReview ? 'read-only' : 'hidden';
  return { people, links, retention };
}

const BOOLS = [false, true] as const;
const AUTHORITIES = ['', 'project_owner'] as const;

describe('artifact access sections', () => {
  it('phase 2 offers stewards Create link in the dialog', () => {
    expect(STEWARD_CREATE_LINK).toBe(true);
  });

  describe('sectionStates over every flag combination', () => {
    for (const canManage of BOOLS) {
      for (const canShare of BOOLS) {
        for (const canGrantReview of BOOLS) {
          for (const shareAuthority of AUTHORITIES) {
            for (const stewardCreateLink of BOOLS) {
              const c: AccessCapabilities = {
                canManage,
                canShare,
                canGrantReview,
                shareAuthority,
              };
              it(`${JSON.stringify(c)} stewardCreateLink=${stewardCreateLink}`, () => {
                expect(sectionStates(c, { stewardCreateLink })).toEqual(
                  expected(c, stewardCreateLink)
                );
                expect(showManageAccess(c)).toBe(canShare || canGrantReview);
              });
            }
          }
        }
      }
    }
  });

  it('matches the design matrix for each role (phase 1)', () => {
    const p1 = { stewardCreateLink: false };
    // R1, R2 manager.
    expect(
      sectionStates(
        { canManage: true, canShare: true, canGrantReview: false, shareAuthority: '' },
        p1
      )
    ).toEqual({ people: 'edit', links: 'edit', retention: 'edit' });
    // R3, R4 steward.
    for (const shareAuthority of ['project_owner', 'delegator'] as const) {
      expect(
        sectionStates(
          { canManage: false, canShare: true, canGrantReview: true, shareAuthority },
          p1
        )
      ).toEqual({ people: 'bounded', links: 'revoke-only', retention: 'edit' });
    }
    // R5, R6 review only.
    expect(
      sectionStates(
        { canManage: false, canShare: false, canGrantReview: true, shareAuthority: '' },
        p1
      )
    ).toEqual({ people: 'review-only', links: 'hidden', retention: 'read-only' });
    // R5u, R7, R8: no button, nothing shown.
    const none: AccessCapabilities = {
      canManage: false,
      canShare: false,
      canGrantReview: false,
      shareAuthority: '',
    };
    expect(showManageAccess(none)).toBe(false);
    expect(sectionStates(none, p1)).toEqual(NO_SECTIONS);
  });

  it('matches the design matrix for each role (phase 2, the dialog option)', () => {
    const p2 = { stewardCreateLink: STEWARD_CREATE_LINK };
    // R1, R2 manager: unchanged.
    expect(
      sectionStates(
        { canManage: true, canShare: true, canGrantReview: false, shareAuthority: '' },
        p2
      )
    ).toEqual({ people: 'edit', links: 'edit', retention: 'edit' });
    // R3, R4 steward: Create link added; people stay bounded.
    for (const shareAuthority of ['project_owner', 'delegator'] as const) {
      expect(
        sectionStates(
          { canManage: false, canShare: true, canGrantReview: true, shareAuthority },
          p2
        )
      ).toEqual({ people: 'bounded', links: 'edit', retention: 'edit' });
    }
    // R5, R6 review only: still no links, so no Create link and no link list.
    expect(
      sectionStates(
        { canManage: false, canShare: false, canGrantReview: true, shareAuthority: '' },
        p2
      )
    ).toEqual({ people: 'review-only', links: 'hidden', retention: 'read-only' });
    // R5u, R7, R8: nothing.
    expect(
      sectionStates(
        { canManage: false, canShare: false, canGrantReview: false, shareAuthority: '' },
        p2
      )
    ).toEqual(NO_SECTIONS);
  });

  describe('capabilitiesFrom', () => {
    const base = { artifact: {} } as unknown as ArtifactResponse;

    it('reads absent flags as false', () => {
      expect(capabilitiesFrom(base)).toEqual({
        canManage: false,
        canShare: false,
        canGrantReview: false,
        shareAuthority: '',
      });
      expect(capabilitiesFrom(null)).toEqual(capabilitiesFrom(base));
      expect(capabilitiesFrom(undefined)).toEqual(capabilitiesFrom(base));
    });

    it('copies the flags and a known steward clause', () => {
      expect(
        capabilitiesFrom({
          ...base,
          canManage: true,
          canShare: true,
          canGrantReview: true,
          shareAuthority: 'delegator',
        })
      ).toEqual({
        canManage: true,
        canShare: true,
        canGrantReview: true,
        shareAuthority: 'delegator',
      });
      expect(capabilitiesFrom({ ...base, shareAuthority: 'project_owner' }).shareAuthority).toBe(
        'project_owner'
      );
    });

    it('drops an unknown steward clause', () => {
      expect(capabilitiesFrom({ ...base, shareAuthority: 'admin' }).shareAuthority).toBe('');
      expect(capabilitiesFrom({ ...base, shareAuthority: 'PROJECT_OWNER' }).shareAuthority).toBe(
        ''
      );
    });

    it('treats only a true flag as set', () => {
      const loose = { ...base, canShare: 'yes', canManage: 1 } as unknown as ArtifactResponse;
      expect(capabilitiesFrom(loose)).toEqual(capabilitiesFrom(base));
    });
  });

  describe('needsAccessData', () => {
    const states = (
      people: PeopleState,
      links: LinksState = 'hidden',
      retention: RetentionState = 'hidden'
    ): SectionStates => ({ people, links, retention });

    it('loads for every state that lists links or grants', () => {
      expect(needsAccessData(states('edit'))).toBe(true);
      expect(needsAccessData(states('bounded'))).toBe(true);
      expect(needsAccessData(states('remove-only'))).toBe(true);
      expect(needsAccessData(states('hidden', 'edit'))).toBe(true);
      expect(needsAccessData(states('hidden', 'revoke-only'))).toBe(true);
    });

    it('loads nothing when only review access or a read-only expiry is shown', () => {
      expect(needsAccessData(states('review-only'))).toBe(false);
      expect(needsAccessData(states('review-only', 'hidden', 'read-only'))).toBe(false);
      expect(needsAccessData(states('hidden', 'hidden', 'edit'))).toBe(false);
      expect(needsAccessData(NO_SECTIONS)).toBe(false);
    });
  });

  it('keeps the earlier modes as presets', () => {
    expect(MODE_PRESETS).toEqual({
      share: { people: 'edit', links: 'edit', retention: 'edit' },
      review: { people: 'review-only', links: 'hidden', retention: 'hidden' },
      retention: { people: 'remove-only', links: 'revoke-only', retention: 'edit' },
    });
    expect(needsAccessData(MODE_PRESETS.share)).toBe(true);
    expect(needsAccessData(MODE_PRESETS.review)).toBe(false);
    expect(needsAccessData(MODE_PRESETS.retention)).toBe(true);
  });
});

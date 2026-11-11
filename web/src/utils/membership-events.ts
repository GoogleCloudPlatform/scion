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
 * Window event announcing that a change made in this tab altered who
 * belongs where: a membership or role edit, or a project created (its
 * creator becomes a member). The hub sends no event for these changes, so
 * code that caches what the session may read (the agent store) listens for
 * this one to revalidate.
 */
export const MEMBERSHIP_CHANGED_EVENT = 'scion:membership-changed';

/** What changed: a project's members or a group's members. */
export interface MembershipChangedDetail {
  kind: 'project' | 'group';
  id: string;
}

/** Announce a successful membership change on `target` (defaults to `window`). */
export function dispatchMembershipChanged(
  detail: MembershipChangedDetail,
  target: EventTarget = window
): void {
  target.dispatchEvent(
    new CustomEvent<MembershipChangedDetail>(MEMBERSHIP_CHANGED_EVENT, { detail })
  );
}

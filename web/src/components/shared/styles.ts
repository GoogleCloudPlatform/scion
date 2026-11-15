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

import { css } from 'lit';

/**
 * Screen-reader-only utility class. Visually hides content while keeping
 * it accessible to assistive technology.
 *
 * Import and spread into a component's `styles` array:
 *   static override styles = [srOnlyStyles, css`...`];
 */
export const srOnlyStyles = css`
  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    padding: 0;
    margin: -1px;
    overflow: hidden;
    clip: rect(0, 0, 0, 0);
    white-space: nowrap;
    border: 0;
  }
`;

/**
 * Pill badge marking a user principal whose status is 'invited' (not yet
 * signed in). Matches the invited status badge palette in admin-users.
 */
export const invitedStatusTagStyles = css`
  .status-badge.invited {
    display: inline-flex;
    align-items: center;
    flex-shrink: 0;
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
    font-size: 0.75rem;
    font-weight: 500;
    line-height: 1.25;
    background: var(--sl-color-primary-100, #dbeafe);
    color: var(--sl-color-primary-700, #1d4ed8);
    border: 1px solid var(--sl-color-primary-200, #bfdbfe);
  }
`;

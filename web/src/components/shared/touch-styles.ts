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
 * Shared touch-target styles for components that render an `sl-menu`.
 *
 * Shoelace's menu-item rows render well under 44px by default. Shoelace's
 * shadow parts can only be styled from the scope that renders them, so
 * there is no single global rule that can reach every `sl-menu-item` in the
 * app — each component that renders one includes this fragment instead.
 */

import { css } from 'lit';

export const touchMenuItemStyles = css`
  @media (max-width: 768px), (pointer: coarse) {
    sl-menu-item::part(base) {
      min-height: 44px;
    }
  }
`;

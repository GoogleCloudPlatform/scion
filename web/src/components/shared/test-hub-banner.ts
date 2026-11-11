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
 * Test-hub banner (ptone/scion#4240).
 *
 * A warning shown above the header on every page, and on the login page,
 * while the hub runs with test identities enabled. It cannot be dismissed:
 * the alert has no `closable` attribute. Its only input is the hub's
 * GET /api/v1/test-infra/status (client/test-infra-status.ts).
 *
 * Hosts use TestHubBannerController, which fetches the status and
 * re-renders the host when it arrives, and render the result of
 * renderTestHubBanner(). The app shell keys the banner on the current path,
 * so every navigation renders a fresh element.
 */

import { css, html, nothing, type ReactiveController, type ReactiveControllerHost } from 'lit';
import type { TemplateResult } from 'lit';

import {
  TEST_INFRA_STATUS_EVENT,
  getTestInfraStatus,
  loadTestInfraStatus,
  type TestInfraStatus,
} from '../../client/test-infra-status.js';

export const TEST_HUB_MEMBER_TEXT = 'TEST HUB: test identities are enabled';
export const TEST_HUB_ADMIN_TEXT =
  'TEST HUB: test admin identities are enabled; this hub can mint hub-admins';
export const TEST_HUB_SUPER_ADMIN_TEXT = `${TEST_HUB_ADMIN_TEXT} and super-admins`;

export interface TestHubBannerContent {
  text: string;
  icon: 'exclamation-triangle' | 'exclamation-octagon';
}

/**
 * The banner's text and icon for a status, or null when no gate is on (no
 * banner). The super-admin kind implies the admin tier.
 */
export function testHubBannerContent(status: TestInfraStatus | null): TestHubBannerContent | null {
  if (!status) return null;
  if (status.testSuperAdmin) {
    return { text: TEST_HUB_SUPER_ADMIN_TEXT, icon: 'exclamation-octagon' };
  }
  if (status.testHubAdmin) {
    return { text: TEST_HUB_ADMIN_TEXT, icon: 'exclamation-triangle' };
  }
  if (status.testIdentities) {
    return { text: TEST_HUB_MEMBER_TEXT, icon: 'exclamation-triangle' };
  }
  return null;
}

/** The banner for a status, or `nothing` (no element at all) when no gate is on. */
export function renderTestHubBanner(
  status: TestInfraStatus | null
): TemplateResult | typeof nothing {
  const content = testHubBannerContent(status);
  if (!content) return nothing;
  return html`
    <sl-alert class="test-hub-banner" variant="warning" open data-testid="test-hub-banner">
      <sl-icon slot="icon" name=${content.icon}></sl-icon>
      <strong>${content.text}</strong>
    </sl-alert>
  `;
}

/** Styles for a host's banner: full width, square corners, never shrunk away. */
export const testHubBannerStyles = css`
  sl-alert.test-hub-banner {
    display: block;
    flex-shrink: 0;
    width: 100%;
  }
  sl-alert.test-hub-banner::part(base) {
    border-radius: 0;
  }
`;

/**
 * Layout for a standalone page (login, invite, onboarding) whose host is a
 * centred flex box: while the banner shows (the host carries `test-hub`, see
 * TestHubBannerController's `standalone` option), the host becomes a column,
 * the banner spans the top in normal flow, and the page content stays
 * centred in the space below it. Nothing is reserved at a fixed height, so a
 * banner that wraps to several lines pushes the content down instead of
 * covering it.
 */
export const standaloneTestHubBannerStyles = css`
  :host([test-hub]) {
    flex-direction: column;
    justify-content: flex-start;
  }
  :host([test-hub]) > sl-alert.test-hub-banner {
    align-self: stretch;
    width: auto;
  }
  :host([test-hub]) > :not(.test-hub-banner) {
    margin-block: auto;
  }
`;

export interface TestHubBannerControllerOptions {
  /**
   * For a standalone page: reflect whether the banner shows as the host's
   * `test-hub` attribute (standaloneTestHubBannerStyles), and retry a failed
   * status fetch after each update, since a standalone page has no
   * navigation of its own to retry on.
   */
  standalone?: boolean;
}

/**
 * Keeps a host's view of the status current: fetches it on connect (once per
 * page load; client/test-infra-status.ts shares the request) and re-renders
 * the host when it arrives.
 */
export class TestHubBannerController implements ReactiveController {
  private readonly host: ReactiveControllerHost & Partial<Element>;
  private readonly standalone: boolean;
  private readonly onStatus = (): void => this.host.requestUpdate();

  constructor(
    host: ReactiveControllerHost & Partial<Element>,
    options: TestHubBannerControllerOptions = {}
  ) {
    this.host = host;
    this.standalone = options.standalone === true;
    host.addController(this);
  }

  /** The status to render; null until it has been fetched. */
  get status(): TestInfraStatus | null {
    return getTestInfraStatus();
  }

  hostConnected(): void {
    window.addEventListener(TEST_INFRA_STATUS_EVENT, this.onStatus);
    this.ensure();
  }

  hostDisconnected(): void {
    window.removeEventListener(TEST_INFRA_STATUS_EVENT, this.onStatus);
  }

  hostUpdated(): void {
    if (!this.standalone) return;
    this.host.toggleAttribute?.('test-hub', testHubBannerContent(this.status) !== null);
    this.ensure();
  }

  /** Fetches the status if it is not known yet (a no-op once it is). */
  ensure(): void {
    if (!getTestInfraStatus()) void loadTestInfraStatus();
  }
}

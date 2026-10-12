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

/**
 * Every selector and state name the async create journey reads, in one
 * place. The agent page and the create form are Lit elements with open
 * shadow roots; Playwright CSS locators pierce them, and the in-page
 * sampler (page-recorder.ts) searches shadow roots itself.
 *
 * Three selectors are stable data-testids. The rest are class or element
 * selectors until the pages get test ids for them; when they do, only this
 * file changes.
 */

import type { Locator, Page } from '@playwright/test';

export const SELECTORS = {
  // Agent page, launch in progress (launch.active).
  launchBanner: '[data-testid="agent-launch-banner"]', // stable
  launchStep: '[data-testid="agent-launch-banner"] .launch-step',
  launchMessage: '[data-testid="agent-launch-banner"] .launch-message',

  // Agent page, a create that did not complete.
  incompleteBanner: '[data-testid="incomplete-create-banner"]', // stable
  launchErrorCode: '[data-testid="launch-error-code"]', // stable
  incompleteMessage: '[data-testid="incomplete-create-banner"] .error-message',
  incompleteActions: '[data-testid="incomplete-create-banner"] .error-actions',

  // Agent page header: lifecycle buttons (read by text) and the phase badge
  // (status = activity when running, else phase).
  headerButtons: '.header-actions sl-button',
  headerStatusBadge: 'scion-detail-header scion-status-badge',

  // Status tab: the info item labelled "Phase" holds a badge whose status
  // attribute is the phase.
  infoItem: '.info-item',
  infoLabel: '.info-label',
  statusBadge: 'scion-status-badge',

  // Generic error banner without a launch (a synchronous create error).
  genericErrorBanner: '.agent-error-banner',

  // Create form.
  createPage: 'scion-page-agent-create',
  createName: 'sl-input#name',
  createTemplate: 'sl-select#template',
  createHarness: 'sl-select#harness',
  createBroker: 'sl-select#broker',
  createTask: 'sl-textarea#task',
  createSubmit: '.form-actions sl-button[variant="primary"]',
  createError: '.form-card .error-banner',

  // Confirm dialog (components/shared/confirm-dialog.ts).
  confirmDialog: 'sl-dialog[open]',
} as const;

/** Header lifecycle buttons that must not be offered for an incomplete create. */
export const FORBIDDEN_HEADER_ACTIONS = ['Start', 'Resume', 'Resume (best effort)'] as const;

/** Agent phases in launch order; the later ones end a launch. */
export const PHASES = [
  'created',
  'provisioning',
  'cloning',
  'starting',
  'running',
  'stopping',
  'stopped',
  'suspended',
  'error',
] as const;

/** Phases of an in-flight create, in order. */
export const LAUNCH_PHASES = ['created', 'provisioning', 'cloning', 'starting', 'running'] as const;

export const LAUNCH_END_REASONS = [
  'succeeded',
  'running_observed',
  'failed',
  'timed_out',
  'lost',
  'not_launched',
  'superseded',
] as const;

/**
 * Launch failure codes the hub may send: today's, plus those of the
 * per-step progress work (pkg/api/launch_codes.go). A code outside the
 * list fails the check, so a new code is a one-line change here.
 */
export const KNOWN_LAUNCH_ERROR_CODES = [
  'launch_timeout',
  'broker_lost',
  'launch_stopped',
  'agent_error',
  'hub_unreachable',
  'runtime_error',
  'name_in_use',
  'template_not_found',
  'harness_config_policy',
  'agent_state_unavailable',
  'agent_state_conflict',
  'image_pull_failed',
  'invalid_image',
  'container_config',
  'crash_loop',
  'init_container_failed',
  'unschedulable',
  'pod_disrupted',
  'broker_restarted',
] as const;

/** Codes expected when the image cannot be pulled (the bad image check). */
export const IMAGE_FAILURE_CODES = ['runtime_error', 'image_pull_failed', 'invalid_image'] as const;

/**
 * The code part of the launch-error-code text ("<code> — <reason>", just
 * "<code>", or just a reason such as "stopping" while the create winds
 * down). Returns "" when the text holds no code.
 */
export function launchErrorCodeOf(text: string): string {
  const head = text.split('—')[0].trim();
  return /^[a-z][a-z0-9_]*$/.test(head) && head.includes('_') ? head : '';
}

/** The visible text of the header lifecycle buttons. */
export async function headerButtonTexts(page: Page): Promise<string[]> {
  const texts = await page.locator(SELECTORS.headerButtons).allInnerTexts();
  return texts.map((t) => t.replace(/\s+/g, ' ').trim()).filter((t) => t.length > 0);
}

/** The phase badge of the Status tab (status attribute = phase). */
export function phaseBadge(page: Page): Locator {
  return page
    .locator(SELECTORS.infoItem)
    .filter({ has: page.locator(SELECTORS.infoLabel, { hasText: /^\s*Phase\s*$/ }) })
    .locator(SELECTORS.statusBadge)
    .first();
}

/** The Delete button of the incomplete create panel. */
export function incompleteDeleteButton(page: Page): Locator {
  return page.locator(`${SELECTORS.incompleteActions} sl-button`).filter({ hasText: 'Delete' });
}

/** The "Create again" link of the incomplete create panel. */
export function incompleteCreateAgainLink(page: Page): Locator {
  return page
    .locator(`${SELECTORS.incompleteActions} a`)
    .filter({ has: page.locator('sl-button', { hasText: 'Create again' }) });
}

/** The confirm button of the open confirm dialog. */
export function confirmDialogButton(page: Page, text = 'Confirm'): Locator {
  return page.locator(`${SELECTORS.confirmDialog} sl-button`).filter({ hasText: text });
}

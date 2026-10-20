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
 * sampler (page-recorder.ts) reads the agent page's shadow root itself.
 *
 * The agent page is read through its data-testids only (launch banner,
 * failed-create panel, header lifecycle controls, Status tab phase). The
 * create form and the confirm dialog have no test ids yet; they keep
 * element selectors.
 */

import type { Locator, Page } from '@playwright/test';

import { LAUNCH_STEP_LABELS, launchStepLabel } from '../../src/shared/agent-launch-labels.js';

/** A data-testid selector. */
function testId(id: string): string {
  return `[data-testid="${id}"]`;
}

export const SELECTORS = {
  // Agent page, launch in progress (launch.active).
  launchBanner: testId('agent-launch-banner'),
  // "Step: <step label>" (see uiStepLabel and uiStepName).
  launchStep: testId('launch-step'),
  launchMessage: testId('launch-message'),

  // Agent page, a create that did not complete.
  incompleteBanner: testId('incomplete-create-banner'),
  // The whole "<code> — <end reason>" line, and its two parts.
  launchErrorCode: testId('launch-error-code'),
  launchCode: testId('launch-code'),
  launchEndReason: testId('launch-end-reason'),
  launchErrorMessage: testId('launch-error-message'),
  incompleteTemplate: testId('incomplete-create-template'),
  incompleteTask: testId('incomplete-create-task'),
  incompleteDelete: testId('incomplete-create-delete'),
  incompleteRecreate: testId('incomplete-create-recreate'),

  // Agent page header lifecycle controls.
  headerStart: testId('header-start'),
  headerResume: testId('header-resume'),
  headerForceResume: testId('header-force-resume'),

  // Status tab phase badge (status attribute = phase).
  agentPhase: testId('agent-phase'),

  // Create form.
  createPage: 'scion-page-agent-create',
  createName: 'sl-input#name',
  createNameInput: 'sl-input#name input',
  createHarness: 'sl-select#harness',
  createBroker: 'sl-select#broker',
  createTask: 'sl-textarea#task',
  createTaskInput: 'sl-textarea#task textarea',
  selectOption: 'sl-option',
  createSubmit: '.form-actions sl-button[variant="primary"]',
  // Additional Options disclosure and its Container image field
  // (agent-config-form.ts, key config.image, General tab).
  createAdvancedHeader: 'sl-details[summary="Additional Options"] [part="header"]',
  createImageInput: 'scion-agent-config-form .field[data-key="config.image"] sl-input input',
  createError: '.form-card .error-banner',

  // Confirm dialog (components/shared/confirm-dialog.ts).
  confirmDialog: 'sl-dialog[open]',
} as const;

/**
 * Header lifecycle controls that must all be absent for an incomplete
 * create (Start, Resume, Resume (best effort)), by test id.
 */
export const FORBIDDEN_HEADER_CONTROLS = {
  'header-start': SELECTORS.headerStart,
  'header-resume': SELECTORS.headerResume,
  'header-force-resume': SELECTORS.headerForceResume,
} as const;

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

/**
 * Launch steps in canonical order (pkg/api/launch_codes.go). A container
 * runtime reports only pre_clean (when a stale container existed) and then
 * launching; claim is never reported as a step.
 */
export const LAUNCH_STEPS = [
  'claim',
  'pre_clean',
  'secrets',
  'pod_create',
  'scheduling',
  'awaiting_capacity',
  'init_containers',
  'container_create',
  'image_pull',
  'container_start',
  'home_sync',
  'workspace_sync',
  'startup_gate',
  'launching',
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
 * per-step progress work (pkg/api/launch_codes.go). Whether the panel's
 * code is in the list is recorded (panel.codeKnown), not checked.
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

/**
 * The launch code expected on a container runtime for an image tag that
 * does not exist (A3f). RECORD ONLY: recorded next to the code seen, never
 * asserted. Typed image codes (image_pull_failed, invalid_image) come from
 * the Kubernetes runtime only.
 */
export const A3F_EXPECTED_CODE = 'runtime_error';

/**
 * The launch-step text the page shows for a step: "Step: " and the step's
 * label (src/shared/agent-launch-labels.ts), e.g. "Step: pulling the image".
 */
export function uiStepLabel(step: string): string {
  return `Step: ${launchStepLabel(step)}`;
}

/**
 * The raw step name of a launch-step text: "Step: pulling the image" gives
 * "image_pull". A text that is no labelled step's label gives the text with
 * spaces as "_", the label of a step without one. Returns "" for an empty
 * text.
 */
export function uiStepName(text: string): string {
  const label = text
    .replace(/^\s*Step:\s*/, '')
    .trim()
    .replace(/\s+/g, ' ');
  if (!label) return '';
  const step = Object.keys(LAUNCH_STEP_LABELS).find((s) => LAUNCH_STEP_LABELS[s] === label);
  return step ?? label.replace(/ /g, '_');
}

/**
 * The header lifecycle controls rendered on the page, and those of them
 * that are visible, by test id.
 */
export async function headerControlsShown(
  page: Page
): Promise<{ rendered: string[]; visible: string[] }> {
  const rendered: string[] = [];
  const visible: string[] = [];
  for (const [id, selector] of Object.entries(FORBIDDEN_HEADER_CONTROLS)) {
    const control = page.locator(selector);
    if ((await control.count()) === 0) continue;
    rendered.push(id);
    if (await control.first().isVisible()) visible.push(id);
  }
  return { rendered, visible };
}

/** The phase badge of the Status tab (status attribute = phase). */
export function phaseBadge(page: Page): Locator {
  return page.locator(SELECTORS.agentPhase).first();
}

/** The Delete button of the incomplete create panel. */
export function incompleteDeleteButton(page: Page): Locator {
  return page.locator(SELECTORS.incompleteDelete);
}

/** The "Create again" link of the incomplete create panel. */
export function incompleteCreateAgainLink(page: Page): Locator {
  return page.locator(SELECTORS.incompleteRecreate);
}

/** The confirm button of the open confirm dialog. */
export function confirmDialogButton(page: Page, text = 'Confirm'): Locator {
  return page.locator(`${SELECTORS.confirmDialog} sl-button`).filter({ hasText: text });
}

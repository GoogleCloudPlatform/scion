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

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

// The Reset Auth card offers the bulk scope re-issue to super-admins only:
// a dry run first, then an Apply step behind a confirmation dialog that
// sends dry_run false.

const RESET_AUTH_PATH = '/api/v1/admin/agents/reset-auth-all';

function reissueResult(dryRun: boolean): Record<string, unknown> {
  return {
    batch_op_id: 'batch-1',
    dry_run: dryRun,
    total: 3,
    succeeded: [{ id: 'a-1', name: 'alpha' }],
    noop: [{ id: 'b-1', name: 'beta' }],
    refused: [{ id: 'c-1', name: 'gamma', cause: 'delegator_missing' }],
    push_failed: null,
    agents: [
      {
        id: 'a-1',
        name: 'alpha',
        project_id: 'p-1',
        depth: 0,
        outcome: 'changed',
        added: ['agent.read'],
        removed: ['project.write'],
        role_before: 'editor',
        role_after: 'viewer',
      },
      { id: 'b-1', name: 'beta', project_id: 'p-1', depth: 0, outcome: 'noop' },
      {
        id: 'c-1',
        name: 'gamma',
        project_id: 'p-1',
        depth: 1,
        outcome: 'refused',
        cause: 'delegator_missing',
      },
    ],
    depth_unresolved: null,
    batch_audit_recorded: true,
  };
}

interface Call {
  path: string;
  body: unknown;
}

let calls: Call[] = [];

interface MountOpts {
  superAdmin: boolean;
  /** Status for every re-issue request (dry run and apply). */
  reissueStatus?: number;
  /** Status for the apply request (dry_run false) only. */
  applyStatus?: number;
  /** When set, the apply response waits for this promise. */
  holdApply?: Promise<void>;
}

function makeHandler(
  opts: MountOpts
): (input: string | URL | Request, init?: RequestInit) => Promise<Response> {
  return async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const path = new URL(url, 'http://localhost').pathname;
    const body = typeof init?.body === 'string' ? (JSON.parse(init.body) as unknown) : undefined;
    calls.push({ path, body });
    let status = 200;
    let out: unknown = {};
    let wait: Promise<void> = Promise.resolve();
    if (path === '/api/v1/auth/admin-status') {
      out = { isAdmin: opts.superAdmin, isSuperAdmin: opts.superAdmin, permissions: [] };
    } else if (path === RESET_AUTH_PATH) {
      const req = (body ?? {}) as { reissue_scopes?: boolean; dry_run?: boolean };
      const apply = req.dry_run === false;
      if (apply && opts.holdApply) wait = opts.holdApply;
      if (opts.reissueStatus && opts.reissueStatus !== 200) {
        status = opts.reissueStatus;
        out = { error: { code: 'forbidden', message: 'hub super-admin required' } };
      } else if (apply && opts.applyStatus && opts.applyStatus !== 200) {
        status = opts.applyStatus;
        out = { error: { code: 'unavailable', message: 'agent list could not be read' } };
      } else {
        out = reissueResult(req.dry_run !== false);
      }
    } else if (path === '/api/v1/admin/maintenance') {
      out = { enabled: false };
    } else if (path.endsWith('/operations')) {
      out = { migrations: [], operations: [] };
    }
    await wait;
    return new Response(JSON.stringify(out), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  };
}

type MaintenancePage = HTMLElement & { updateComplete: Promise<unknown> };

let el: MaintenancePage | null = null;
let clearAdminStatus: () => void = () => {};
let setUser: (id: string) => void = () => {};

beforeAll(async () => {
  await import('./admin-maintenance.js');
  const adminStatus = await import('../../client/admin-status.js');
  clearAdminStatus = adminStatus.clearAdminStatus;
  const { stateManager } = await import('../../client/state.js');
  setUser = (id) => stateManager.setCurrentUserId(id);
}, 60_000);

beforeEach((): void => {
  calls = [];
  clearAdminStatus();
  setUser('user-1');
});

afterEach(() => {
  el?.remove();
  el = null;
  clearAdminStatus();
  vi.unstubAllGlobals();
});

async function settle(page: MaintenancePage, rounds = 10): Promise<void> {
  for (let i = 0; i < rounds; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
  }
}

async function mount(opts: MountOpts): Promise<MaintenancePage> {
  vi.stubGlobal('fetch', vi.fn(makeHandler(opts)));
  el = document.createElement('scion-page-admin-maintenance') as MaintenancePage;
  document.body.appendChild(el);
  await settle(el);
  return el;
}

function q(page: MaintenancePage, sel: string): HTMLElement | null {
  return page.shadowRoot?.querySelector<HTMLElement>(sel) ?? null;
}

function reissueCalls(): Call[] {
  return calls.filter(
    (c) => c.path === RESET_AUTH_PATH && (c.body as { reissue_scopes?: boolean })?.reissue_scopes
  );
}

describe('admin maintenance reset auth: re-issue scopes', () => {
  it('hides the re-issue scopes option for a user who is not a super-admin', async () => {
    const page = await mount({ superAdmin: false });
    expect(q(page, '.card-title')).not.toBeNull();
    expect(q(page, '.reissue-scopes')).toBeNull();
    expect(q(page, '.reissue-dry-run')).toBeNull();
  });

  it('shows the re-issue scopes option for a super-admin', async () => {
    const page = await mount({ superAdmin: true });
    expect(q(page, '.reissue-scopes')).not.toBeNull();
    expect(q(page, '.reissue-dry-run')).not.toBeNull();
    // No Apply step before a dry run.
    expect(q(page, '.reissue-apply')).toBeNull();
    expect(reissueCalls()).toHaveLength(0);
  });

  it('sends a dry run request first and renders its result', async () => {
    const page = await mount({ superAdmin: true });
    q(page, '.reissue-dry-run')!.click();
    await settle(page);

    const sent = reissueCalls();
    expect(sent).toHaveLength(1);
    expect(sent[0].body).toEqual({ reissue_scopes: true, dry_run: true });

    expect(q(page, '.reissue-summary')?.textContent).toContain(
      '3 agents: 1 would change, 1 unchanged, 1 refused, 0 token not delivered'
    );
    expect(q(page, '.reissue-dry-run-note')?.textContent).toContain('nothing was changed');
    const log = q(page, '.reissue-agents')?.textContent ?? '';
    expect(log).toContain('alpha (a-1) changed');
    expect(log).toContain('role:     editor -> viewer');
    expect(log).toContain('added:    agent.read');
    expect(log).toContain('removed:  project.write');
    expect(log).toContain('gamma (c-1) refused: delegator_missing');
    expect(log).not.toContain('beta');
    expect(q(page, '.reissue-apply')).not.toBeNull();
  });

  it('applies only after confirmation and sends dry_run false', async () => {
    let release: () => void = () => {};
    const holdApply = new Promise<void>((r) => {
      release = r;
    });
    const page = await mount({ superAdmin: true, holdApply });
    q(page, '.reissue-dry-run')!.click();
    await settle(page);

    // Apply opens the confirmation; nothing is sent yet.
    q(page, '.reissue-apply')!.click();
    await settle(page);
    expect(q(page, 'sl-dialog.reissue-confirm')).not.toBeNull();
    expect(reissueCalls()).toHaveLength(1);

    // Cancel sends nothing.
    q(page, '.reissue-cancel')!.click();
    await settle(page);
    expect(q(page, 'sl-dialog.reissue-confirm')).toBeNull();
    expect(reissueCalls()).toHaveLength(1);

    q(page, '.reissue-apply')!.click();
    await settle(page);
    q(page, '.reissue-confirm-apply')!.click();
    await settle(page);

    // While the apply request is in flight, a second Apply sends nothing
    // and the dialog refuses to close.
    expect(reissueCalls()).toHaveLength(2);
    q(page, '.reissue-confirm-apply')!.click();
    await settle(page);
    expect(reissueCalls()).toHaveLength(2);
    const closeEvent = new Event('sl-request-close', { cancelable: true });
    q(page, 'sl-dialog.reissue-confirm')!.dispatchEvent(closeEvent);
    await settle(page);
    expect(closeEvent.defaultPrevented).toBe(true);
    expect(q(page, 'sl-dialog.reissue-confirm')).not.toBeNull();

    release();
    await settle(page);

    const sent = reissueCalls();
    expect(sent).toHaveLength(2);
    expect(sent[1].body).toEqual({ reissue_scopes: true, dry_run: false });
    expect(q(page, 'sl-dialog.reissue-confirm')).toBeNull();
    expect(q(page, '.reissue-summary')?.textContent).toContain(
      '3 agents: 1 changed, 1 unchanged, 1 refused, 0 token not delivered'
    );
    expect(q(page, '.reissue-dry-run-note')).toBeNull();
    expect(q(page, '.reissue-apply')).toBeNull();
  });

  it('renders an apply error and sends exactly one dry_run false request', async () => {
    const page = await mount({ superAdmin: true, applyStatus: 503 });
    q(page, '.reissue-dry-run')!.click();
    await settle(page);
    q(page, '.reissue-apply')!.click();
    await settle(page);
    q(page, '.reissue-confirm-apply')!.click();
    await settle(page);

    const applies = reissueCalls().filter(
      (c) => (c.body as { dry_run?: boolean }).dry_run === false
    );
    expect(applies).toHaveLength(1);
    expect(reissueCalls()).toHaveLength(2);
    const err = q(page, '.reissue-error')?.textContent ?? '';
    expect(err).toContain('Apply failed');
    expect(err).toContain('agent list could not be read');
    expect(q(page, 'sl-dialog.reissue-confirm')).toBeNull();
    expect(q(page, '.reissue-summary')).toBeNull();
    expect(q(page, '.reissue-apply')).toBeNull();
  });

  it('renders a request error in the card', async () => {
    const page = await mount({ superAdmin: true, reissueStatus: 403 });
    q(page, '.reissue-dry-run')!.click();
    await settle(page);

    const err = q(page, '.reissue-error')?.textContent ?? '';
    expect(err).toContain('Dry run failed');
    expect(err).toContain('hub super-admin required');
    expect(q(page, '.reissue-summary')).toBeNull();
    expect(q(page, '.reissue-apply')).toBeNull();
  });
});

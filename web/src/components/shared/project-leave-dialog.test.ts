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
 * The shared Leave project dialog (ptone/scion#7494): the dry-run preview
 * variants, each refusal, delete-first (a failed or unconfirmed delete sends
 * no leave; a confirmed set sends exactly one), and the after-leave steps.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/navigation.js', async (orig) => ({
  ...(await orig<typeof import('../../client/navigation.js')>()),
  navigateTo: vi.fn(),
}));
vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));
vi.mock('./confirm-dialog.js', () => ({ showConfirm: vi.fn(() => Promise.resolve(true)) }));

const sm = vi.hoisted(() => ({ forgetProject: vi.fn(), applyDeleteAccepted: vi.fn(() => true) }));
vi.mock('../../client/state.js', async (orig) => ({
  ...(await orig<typeof import('../../client/state.js')>()),
  stateManager: sm,
}));

import {
  ScionProjectLeaveDialog,
  LEAVE_GROUP_DERIVED_GUIDANCE,
  LEAVE_LAST_OWNER_GUIDANCE,
  LEAVE_TRANSFER_OWNERSHIP_EVENT,
  LEAVE_TRUNCATED_CLOSE_HINT,
  LEAVE_TRUNCATED_NOTE,
  PROJECT_LEFT_EVENT,
  accessKeptLine,
} from './project-leave-dialog.js';
import {
  LEAVE_EXPERIMENT_OFF_MESSAGE,
  LEAVE_NOT_MEMBER_MESSAGE,
  LEAVE_SESSION_REQUIRED_MESSAGE,
  LEAVE_UNSUPPORTED_MESSAGE,
  type LeaveAgentDeleter,
  type LeaveResult,
} from '../../client/project-leave.js';
import { navigateTo } from '../../client/navigation.js';
import { showToast } from '../../utils/toast.js';
import { MEMBERSHIP_CHANGED_EVENT } from '../../utils/membership-events.js';
import { requestBodyText, requestUrl } from '../../client/__fixtures__/request-url.js';

function json(status: number, body: unknown): Response {
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function agentsOf(
  n: number,
  opts: { truncated?: boolean; count?: number } = {}
): LeaveResult['agents'] {
  return {
    count: opts.count ?? n,
    truncated: opts.truncated ?? false,
    items: Array.from({ length: n }, (_, i) => ({
      id: `a${i + 1}`,
      name: `agent-${i + 1}`,
      phase: 'running',
    })),
  };
}

function result(extra: Partial<LeaveResult> = {}): LeaveResult {
  return {
    projectId: 'p-1',
    dryRun: true,
    removedRoles: ['project-member'],
    remainingAccess: 'none',
    agents: agentsOf(2),
    projectBoundTokens: { revoked: 3 },
    willBeHeld: true,
    ...extra,
  };
}

interface Call {
  method: string;
  url: string;
  body: unknown;
}

type Answer = Response | ((call: Call) => Response);

/**
 * Route fetch: the dry run and the real leave by their body, everything
 * else by method and URL. Unmatched requests fail the test.
 */
function routeFetch(routes: {
  dryRun?: Answer;
  leave?: Answer;
  other?: (call: Call) => Response | undefined;
}): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const text = init?.body === undefined ? undefined : requestBodyText(init.body);
      const call: Call = {
        method: init?.method ?? 'GET',
        url: requestUrl(input),
        body: text ? JSON.parse(text) : undefined,
      };
      calls.push(call);
      const pick = (a: Answer | undefined): Response | undefined =>
        typeof a === 'function' ? a(call) : a?.clone();
      let res: Response | undefined;
      if (call.method === 'POST' && call.url.endsWith('/leave')) {
        res = pick((call.body as { dryRun: boolean }).dryRun ? routes.dryRun : routes.leave);
      } else {
        res = routes.other?.(call);
      }
      if (!res) return Promise.reject(new Error(`unexpected ${call.method} ${call.url}`));
      return Promise.resolve(res);
    })
  );
  return calls;
}

const leavePosts = (calls: Call[]): Call[] =>
  calls.filter(
    (c) =>
      c.method === 'POST' && c.url.endsWith('/leave') && !(c.body as { dryRun: boolean }).dryRun
  );

const mounted: HTMLElement[] = [];

async function openDialog(
  opts: { transferAvailable?: boolean } = {}
): Promise<ScionProjectLeaveDialog> {
  const el = new ScionProjectLeaveDialog();
  el.projectId = 'p-1';
  el.projectName = 'Project One';
  el.transferAvailable = opts.transferAvailable ?? false;
  el.waitOptions = { now: (): number => 0, sleep: (): Promise<void> => Promise.resolve() };
  document.body.appendChild(el);
  mounted.push(el);
  await el.show();
  await el.updateComplete;
  return el;
}

const q = (el: HTMLElement, sel: string): HTMLElement | null =>
  el.shadowRoot!.querySelector<HTMLElement>(sel);
const text = (el: HTMLElement): string => el.shadowRoot!.textContent!.replace(/\s+/g, ' ');

async function checkDeleteAgents(el: ScionProjectLeaveDialog): Promise<void> {
  const box = q(el, '[data-leave-delete-agents]') as HTMLInputElement;
  box.checked = true;
  box.dispatchEvent(new Event('sl-change'));
  await el.updateComplete;
}

async function confirm(el: ScionProjectLeaveDialog): Promise<void> {
  q(el, '[data-leave-confirm]')!.click();
  await vi.waitFor(() =>
    expect(['deleting', 'leaving']).not.toContain((el as unknown as { phase: string }).phase)
  );
  await el.updateComplete;
}

beforeEach(() => {
  vi.mocked(navigateTo).mockClear();
  vi.mocked(showToast).mockClear();
  sm.forgetProject.mockClear();
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  mounted.splice(0).forEach((el) => el.remove());
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('dry-run preview', () => {
  it('runs the dry run on open and renders roles, agents, tokens, secrets and steward lines', async () => {
    const calls = routeFetch({ dryRun: json(200, result()) });
    const el = await openDialog();
    expect(calls).toEqual([
      { method: 'POST', url: '/api/v1/projects/p-1/leave', body: { dryRun: true } },
    ]);
    const t = text(el);
    expect(t).toContain("You'll lose access to this project");
    expect(q(el, '[data-leave-roles]')?.textContent).toContain('project-member');
    expect(q(el, '[data-leave-agents]')?.textContent).toContain('2 of your agents');
    expect(q(el, '[data-leave-agent-list]')?.textContent).toContain('agent-1');
    expect(q(el, '[data-leave-tokens]')?.textContent).toContain('3 access tokens');
    expect(q(el, '[data-leave-secrets]')).not.toBeNull();
    expect(q(el, '[data-leave-steward]')).not.toBeNull();
    expect(q(el, '[data-leave-confirm]')?.getAttribute('variant')).toBe('danger');
    expect(q(el, '[data-leave-delete-agents]')?.hasAttribute('disabled')).toBe(false);
  });

  it('with no agents and no tokens, drops those bullets', async () => {
    routeFetch({
      dryRun: json(
        200,
        result({ agents: agentsOf(0), projectBoundTokens: { revoked: 0 }, willBeHeld: false })
      ),
    });
    const el = await openDialog();
    expect(q(el, '[data-leave-agents]')).toBeNull();
    expect(q(el, '[data-leave-tokens]')).toBeNull();
    expect(q(el, '[data-leave-secrets]')).not.toBeNull();
  });

  it('a truncated list shows +N more and disables the delete checkbox', async () => {
    routeFetch({
      dryRun: json(200, result({ agents: agentsOf(50, { truncated: true, count: 57 }) })),
    });
    const el = await openDialog();
    expect(q(el, '[data-leave-more]')?.textContent).toContain('+7 more');
    expect(q(el, '[data-leave-delete-agents]')?.hasAttribute('disabled')).toBe(true);
    expect(q(el, '[data-leave-truncated]')?.textContent).toContain(LEAVE_TRUNCATED_NOTE);
  });

  it('remainingAccess group shows the access-kept line without the agents bullet', async () => {
    routeFetch({ dryRun: json(200, result({ remainingAccess: 'group', willBeHeld: false })) });
    const el = await openDialog();
    expect(q(el, '[data-leave-kept]')?.textContent).toContain(accessKeptLine('group'));
    expect(q(el, '[data-leave-agents]')).toBeNull();
    expect(q(el, '[data-leave-intro]')).toBeNull();
  });

  it('remainingAccess hub shows the hub access-kept line', async () => {
    routeFetch({ dryRun: json(200, result({ remainingAccess: 'hub', willBeHeld: false })) });
    const el = await openDialog();
    expect(q(el, '[data-leave-kept]')?.textContent).toContain(accessKeptLine('hub'));
    expect(q(el, '[data-leave-agents]')).toBeNull();
  });
});

describe('refusals', () => {
  it('409 last_owner shows guidance and a Transfer ownership link that dispatches the event', async () => {
    routeFetch({
      dryRun: json(409, {
        error: { code: 'last_owner', message: 'last owner', details: { guidance: 'POST …' } },
      }),
    });
    const el = await openDialog({ transferAvailable: true });
    expect(q(el, '[data-leave-refusal="last_owner"]')?.textContent).toContain(
      LEAVE_LAST_OWNER_GUIDANCE
    );
    const onTransfer = vi.fn();
    el.addEventListener(LEAVE_TRANSFER_OWNERSHIP_EVENT, onTransfer);
    q(el, '[data-leave-transfer]')!.click();
    expect(onTransfer).toHaveBeenCalledTimes(1);
    expect(q(el, '[data-leave-confirm]')).toBeNull();
  });

  it('409 last_owner without transfer authority has no link', async () => {
    routeFetch({ dryRun: json(409, { error: { code: 'last_owner', message: 'x' } }) });
    const el = await openDialog({ transferAvailable: false });
    expect(q(el, '[data-leave-transfer]')).toBeNull();
  });

  it('409 membership_group_derived lists only details.groups', async () => {
    routeFetch({
      dryRun: json(409, {
        error: {
          code: 'membership_group_derived',
          message: 'through a group',
          details: { groups: [{ id: 'g-1', name: 'Engineering' }] },
        },
      }),
    });
    const el = await openDialog();
    expect(q(el, '[data-leave-refusal="group_derived"]')?.textContent).toContain(
      LEAVE_GROUP_DERIVED_GUIDANCE
    );
    const groups = q(el, '[data-leave-groups]')!.querySelectorAll('li');
    expect(Array.from(groups).map((li) => li.textContent)).toEqual(['Engineering']);
    expect(q(el, '[data-leave-confirm]')).toBeNull();
  });

  it('403 session_required shows the login-session message', async () => {
    routeFetch({
      dryRun: json(403, {
        error: {
          code: 'forbidden',
          message: 'requires an interactive session',
          details: { credential: 'session_required' },
        },
      }),
    });
    const el = await openDialog();
    expect(q(el, '[data-leave-refusal="session_required"]')?.textContent).toContain(
      LEAVE_SESSION_REQUIRED_MESSAGE
    );
  });

  it('404 shows no longer a member', async () => {
    routeFetch({
      dryRun: json(404, { error: { code: 'not_found', message: 'Project not found.' } }),
    });
    const el = await openDialog();
    expect(q(el, '[data-leave-refusal="not_member"]')?.textContent).toContain(
      LEAVE_NOT_MEMBER_MESSAGE
    );
  });

  it('a refusal on the real leave renders the same refusal and leaves nothing behind', async () => {
    routeFetch({
      dryRun: json(200, result({ agents: agentsOf(0) })),
      leave: json(409, { error: { code: 'last_owner', message: 'x' } }),
    });
    const el = await openDialog();
    await confirm(el);
    expect(q(el, '[data-leave-refusal="last_owner"]')).not.toBeNull();
    expect(showToast).not.toHaveBeenCalled();
    expect(navigateTo).not.toHaveBeenCalled();
  });
});

describe('delete-first', () => {
  it('planted positive: a confirmed set deletes each agent, then sends exactly one leave', async () => {
    const order: string[] = [];
    const calls = routeFetch({
      dryRun: json(200, result()),
      leave: () => {
        order.push('leave');
        return json(200, result({ dryRun: false }));
      },
    });
    const el = await openDialog();
    el.deleter = ((id) => {
      order.push(`delete ${id}`);
      return Promise.resolve({ kind: 'confirmed' });
    }) as LeaveAgentDeleter;
    await checkDeleteAgents(el);
    await confirm(el);
    expect(order).toEqual(['delete a1', 'delete a2', 'leave']);
    expect(leavePosts(calls)).toHaveLength(1);
  });

  it('a failed delete stops the flow, names the agent, and sends no leave', async () => {
    const calls = routeFetch({ dryRun: json(200, result()), leave: json(200, result()) });
    const el = await openDialog();
    const deleter = vi.fn<LeaveAgentDeleter>((id) =>
      Promise.resolve(
        id === 'a1'
          ? { kind: 'failed', reason: 'delete_failed', message: 'broker down' }
          : { kind: 'confirmed' }
      )
    );
    el.deleter = deleter;
    await checkDeleteAgents(el);
    await confirm(el);
    expect(deleter).toHaveBeenCalledTimes(1);
    expect(q(el, '[data-leave-delete-failed]')?.textContent).toContain('agent-1');
    expect(q(el, '[data-leave-delete-failed]')?.textContent).toContain('broker down');
    expect(q(el, '[data-leave-retry]')).not.toBeNull();
    expect(q(el, '[data-leave-cancel]')).not.toBeNull();
    expect(leavePosts(calls)).toHaveLength(0);
    expect(navigateTo).not.toHaveBeenCalled();
  });

  it('an unconfirmed delete through the shared helper (202, then not taken) sends no leave', async () => {
    const calls = routeFetch({
      dryRun: json(200, result({ agents: agentsOf(1) })),
      leave: json(200, result()),
      other: (c) => {
        if (c.method === 'DELETE' && c.url === '/api/v1/agents/a1') {
          return json(202, {
            agentId: 'a1',
            deletion: { state: 'deleting', soft: false, startedAt: 'x' },
          });
        }
        if (c.method === 'GET' && c.url === '/api/v1/agents/a1') {
          return json(200, { id: 'a1', deletion: null });
        }
        return undefined;
      },
    });
    const el = await openDialog();
    await checkDeleteAgents(el);
    await confirm(el);
    expect(calls.filter((c) => c.method === 'GET').length).toBeGreaterThanOrEqual(2);
    expect(q(el, '[data-leave-delete-failed]')?.textContent).toContain('agent-1');
    expect(leavePosts(calls)).toHaveLength(0);
  });

  it('planted positive through the shared helper: 202 then 404 sends exactly one leave', async () => {
    const calls = routeFetch({
      dryRun: json(200, result({ agents: agentsOf(1) })),
      leave: json(200, result({ dryRun: false })),
      other: (c) => {
        if (c.method === 'DELETE' && c.url === '/api/v1/agents/a1') {
          return json(202, {
            agentId: 'a1',
            deletion: { state: 'deleting', soft: false, startedAt: 'x' },
          });
        }
        if (c.method === 'GET' && c.url === '/api/v1/agents/a1') return json(404, {});
        return undefined;
      },
    });
    const el = await openDialog();
    await checkDeleteAgents(el);
    await confirm(el);
    expect(leavePosts(calls)).toHaveLength(1);
  });

  it('Retry resumes from the failed agent and then leaves once', async () => {
    const calls = routeFetch({
      dryRun: json(200, result()),
      leave: json(200, result({ dryRun: false })),
    });
    const el = await openDialog();
    const attempts: string[] = [];
    let failOnce = true;
    el.deleter = ((id) => {
      attempts.push(id);
      if (id === 'a2' && failOnce) {
        failOnce = false;
        return Promise.resolve({ kind: 'failed', reason: 'timed_out', message: 'slow' });
      }
      return Promise.resolve({ kind: 'confirmed' });
    }) as LeaveAgentDeleter;
    await checkDeleteAgents(el);
    await confirm(el);
    expect(leavePosts(calls)).toHaveLength(0);
    q(el, '[data-leave-retry]')!.click();
    await vi.waitFor(() => expect(leavePosts(calls)).toHaveLength(1));
    expect(attempts).toEqual(['a1', 'a2', 'a2']);
  });

  it('without the checkbox no agent is deleted', async () => {
    const calls = routeFetch({
      dryRun: json(200, result()),
      leave: json(200, result({ dryRun: false })),
    });
    const el = await openDialog();
    const deleter = vi.fn<LeaveAgentDeleter>();
    el.deleter = deleter;
    await confirm(el);
    expect(deleter).not.toHaveBeenCalled();
    expect(leavePosts(calls)).toHaveLength(1);
  });
});

describe('after leaving', () => {
  it('remainingAccess none: toast, project dropped from the cache, membership event, /projects', async () => {
    routeFetch({
      dryRun: json(200, result({ agents: agentsOf(0) })),
      leave: json(200, result({ dryRun: false, agents: agentsOf(0) })),
    });
    const el = await openDialog();
    const membership = vi.fn();
    const left = vi.fn();
    window.addEventListener(MEMBERSHIP_CHANGED_EVENT, membership);
    el.addEventListener(PROJECT_LEFT_EVENT, left);
    await confirm(el);
    window.removeEventListener(MEMBERSHIP_CHANGED_EVENT, membership);
    expect(showToast).toHaveBeenCalledWith('You left Project One', 'success');
    expect(sm.forgetProject).toHaveBeenCalledWith('p-1');
    expect(navigateTo).toHaveBeenCalledWith('/projects');
    expect(membership).toHaveBeenCalledTimes(1);
    expect(left).toHaveBeenCalledTimes(1);
    expect(q(el, 'sl-dialog')).toBeNull();
  });

  it('remainingAccess group: toast, stays (no navigation, no cache drop), host notified', async () => {
    routeFetch({
      dryRun: json(200, result({ remainingAccess: 'group' })),
      leave: json(200, result({ dryRun: false, remainingAccess: 'group' })),
    });
    const el = await openDialog();
    const left = vi.fn();
    el.addEventListener(PROJECT_LEFT_EVENT, left);
    await confirm(el);
    expect(showToast).toHaveBeenCalledWith('You left Project One', 'success');
    expect(navigateTo).not.toHaveBeenCalled();
    expect(sm.forgetProject).not.toHaveBeenCalled();
    expect((left.mock.calls[0][0] as CustomEvent).detail.result.remainingAccess).toBe('group');
  });
});

describe('round-1 review: a truncated list, Retry after a failed leave, not a member, busy', () => {
  it('a checked box on a truncated list (state forced past the disabled control) sends no leave', async () => {
    const calls = routeFetch({
      dryRun: json(200, result({ agents: agentsOf(50, { truncated: true, count: 57 }) })),
      leave: json(200, result({ dryRun: false })),
    });
    const el = await openDialog();
    const deleter = vi.fn<LeaveAgentDeleter>();
    el.deleter = deleter;
    await checkDeleteAgents(el);
    await confirm(el);
    expect(leavePosts(calls)).toHaveLength(0);
    expect(deleter).not.toHaveBeenCalled();
    expect(q(el, '[data-leave-delete-failed]')?.textContent).toContain(LEAVE_TRUNCATED_NOTE);
    // The way on is to close and leave without deleting; Cancel is the only action.
    expect(q(el, '[data-leave-truncated-hint]')?.textContent).toContain(LEAVE_TRUNCATED_CLOSE_HINT);
    expect(q(el, '[data-leave-retry]')).toBeNull();
    expect(q(el, '[data-leave-confirm]')).toBeNull();
    expect(q(el, '[data-leave-cancel]')).not.toBeNull();
  });

  it('planted positive beside it: the same flow on a whole list sends exactly one leave', async () => {
    const calls = routeFetch({
      dryRun: json(200, result({ agents: agentsOf(2) })),
      leave: json(200, result({ dryRun: false })),
    });
    const el = await openDialog();
    el.deleter = (() => Promise.resolve({ kind: 'confirmed' })) as LeaveAgentDeleter;
    await checkDeleteAgents(el);
    await confirm(el);
    expect(leavePosts(calls)).toHaveLength(1);
  });

  it('a leave that fails, then Retry: a fresh dry run, one leave after confirming again, no repeat deletes', async () => {
    let leaveCount = 0;
    let dryCount = 0;
    const calls = routeFetch({
      dryRun: () => {
        dryCount++;
        // After the deletes, the hub lists no agents.
        return json(200, result(dryCount === 1 ? {} : { agents: agentsOf(0), willBeHeld: false }));
      },
      leave: () => {
        leaveCount++;
        return leaveCount === 1
          ? json(500, { error: { code: 'internal_error', message: 'boom' } })
          : json(200, result({ dryRun: false, agents: agentsOf(0) }));
      },
    });
    const el = await openDialog();
    const deleter = vi.fn<LeaveAgentDeleter>(() => Promise.resolve({ kind: 'confirmed' }));
    el.deleter = deleter;
    await checkDeleteAgents(el);
    await confirm(el);
    expect(q(el, '[data-leave-refusal="error"]')).not.toBeNull();
    expect(deleter).toHaveBeenCalledTimes(2);
    const before = calls.length;

    q(el, '[data-leave-retry]')!.click();
    await vi.waitFor(() => expect(q(el, '[data-leave-confirm]')).not.toBeNull());
    const next = calls[before];
    expect(next).toEqual({
      method: 'POST',
      url: '/api/v1/projects/p-1/leave',
      body: { dryRun: true },
    });

    await confirm(el);
    const after = calls.slice(before);
    expect(after.filter((c) => (c.body as { dryRun: boolean }).dryRun === false)).toHaveLength(1);
    expect(deleter).toHaveBeenCalledTimes(2);
    expect(navigateTo).toHaveBeenCalledWith('/projects');
  });

  it('not a member on the dry run: the project is dropped from the cache and closing goes to /projects', async () => {
    routeFetch({
      dryRun: json(404, { error: { code: 'not_found', message: 'Project not found.' } }),
    });
    const el = await openDialog();
    expect(q(el, '[data-leave-refusal="not_member"]')?.textContent).toContain(
      LEAVE_NOT_MEMBER_MESSAGE
    );
    expect(sm.forgetProject).toHaveBeenCalledWith('p-1');
    expect(navigateTo).not.toHaveBeenCalled();
    q(el, '[data-leave-cancel]')!.click();
    await el.updateComplete;
    expect(navigateTo).toHaveBeenCalledWith('/projects');
    expect(q(el, 'sl-dialog')).toBeNull();
  });

  it('not a member on the real leave: the same', async () => {
    routeFetch({
      dryRun: json(200, result({ agents: agentsOf(0) })),
      leave: json(404, { error: { code: 'not_found', message: 'Project not found.' } }),
    });
    const el = await openDialog();
    await confirm(el);
    expect(sm.forgetProject).toHaveBeenCalledWith('p-1');
    q(el, '[data-leave-cancel]')!.click();
    expect(navigateTo).toHaveBeenCalledWith('/projects');
  });

  it('the experiment off (the router 404) is not "not a member": no cache drop, no navigation', async () => {
    routeFetch({ dryRun: json(404, { error: { code: 'not_found', message: 'route not found' } }) });
    const el = await openDialog();
    expect(q(el, '[data-leave-refusal="unavailable"]')?.textContent).toContain(
      LEAVE_EXPERIMENT_OFF_MESSAGE
    );
    q(el, '[data-leave-cancel]')!.click();
    expect(sm.forgetProject).not.toHaveBeenCalled();
    expect(navigateTo).not.toHaveBeenCalled();
  });

  it('a hub without the leave route is not "not a member" either', async () => {
    routeFetch({
      dryRun: json(404, { error: { code: 'not_found', message: 'Project route not found' } }),
    });
    const el = await openDialog();
    expect(q(el, '[data-leave-refusal="unavailable"]')?.textContent).toContain(
      LEAVE_UNSUPPORTED_MESSAGE
    );
    expect(sm.forgetProject).not.toHaveBeenCalled();
  });

  it('show() while a delete runs sends no new dry run', async () => {
    const calls = routeFetch({ dryRun: json(200, result()), leave: json(200, result()) });
    const el = await openDialog();
    let release!: () => void;
    el.deleter = (() =>
      new Promise((resolve) => {
        release = () => resolve({ kind: 'failed', reason: 'timed_out', message: 'slow' });
      })) as LeaveAgentDeleter;
    await checkDeleteAgents(el);
    q(el, '[data-leave-confirm]')!.click();
    await vi.waitFor(() => expect(q(el, '[data-leave-deleting]')).not.toBeNull());
    const before = calls.length;
    await el.show();
    expect(calls.length).toBe(before);
    expect(q(el, '[data-leave-deleting]')).not.toBeNull();
    release();
    await vi.waitFor(() => expect(q(el, '[data-leave-delete-failed]')).not.toBeNull());
    expect(leavePosts(calls)).toHaveLength(0);
  });
});

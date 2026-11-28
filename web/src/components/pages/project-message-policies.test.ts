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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { resetServerFlagStateForTests, setServerFlags } from '../../utils/feature-flags.js';
import './project-message-policies.js';
import type { ScionPageProjectMessagePolicies } from './project-message-policies.js';
import type { ScionMessagePolicyList } from '../message-policies/policy-list.js';
import type { ScionMessagePolicyOutcomes } from '../message-policies/policy-outcomes.js';
import './project-detail.js';
import type { ScionPageProjectDetail } from './project-detail.js';
import '../app-shell.js';
import type { ScionAppShell } from '../app-shell.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const SAMPLE_COVERAGE = {
  enforced_surfaces: ['agent_to_user', 'agent_to_conversation', 'agent_dm', 'mention_delivery'],
  uncovered_agent_paths: ['group_post', 'group_set', 'mention_fanout', 'broadcast'],
};

const SAMPLE_POLICIES = [
  {
    id: 'pol-1',
    project_id: 'proj-1',
    name: 'no-prose-choices',
    description: 'Require structured choices',
    enabled: true,
    mode: 'enforce',
    scope: {
      surfaces: ['agent_to_user', 'agent_to_conversation'],
      sender: { all_agents: true },
      recipient: { kinds: ['user', 'conversation'] },
    },
    violation_rule: "!message.has_choices && message.body.contains('Option A')",
    on_failed: 'reject',
    reject_reason: 'Prose choices are not permitted.',
    reject_remediation: 'Use structured choices.',
    revision: 3,
    created_at: '2026-11-27T01:00:00Z',
    updated_at: '2026-11-27T02:00:00Z',
  },
  {
    id: 'pol-2',
    project_id: 'proj-1',
    name: 'topic-guard',
    description: 'Keep DMs on topic',
    enabled: true,
    mode: 'shadow',
    scope: {
      surfaces: ['agent_dm'],
      sender: { all_agents: true },
    },
    violation_rule: "judgment_labels.on_topic == 'out_of_scope'",
    system_one_rule: '{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1}}',
    system_one_refs: ['gemini-main'],
    on_uncertain: 'reject',
    on_failed: 'admit',
    on_failed_reasons: { timeout: 'admit' },
    revision: 1,
    created_at: '2026-11-26T01:00:00Z',
    updated_at: '2026-11-26T12:00:00Z',
  },
];

const SAMPLE_DECISIONS_PAGE_1 = [
  {
    id: 'dec-1',
    project_id: 'proj-1',
    policy_id: 'pol-1',
    policy_name: 'no-prose-choices',
    policy_revision: 3,
    mode: 'enforce',
    surface: 'agent_to_user',
    sender_kind: 'agent',
    sender_id: 'agent-1',
    recipient_kind: 'user',
    recipient_id: 'user-1',
    class: 'violation',
    action: 'rejected',
    reason: 'policy_rejected',
    elapsed_ms: 4,
    created_at: '2026-11-27T10:02:11Z',
  },
  {
    id: 'dec-2',
    project_id: 'proj-1',
    policy_id: 'pol-2',
    policy_name: 'topic-guard',
    policy_revision: 1,
    mode: 'shadow',
    surface: 'agent_dm',
    sender_kind: 'agent',
    sender_id: 'agent-1',
    recipient_kind: 'agent',
    recipient_id: 'agent-2',
    message_id: 'msg-admitted-99',
    class: 'pass',
    action: 'admitted',
    elapsed_ms: 120,
    created_at: '2026-11-27T09:58:40Z',
    evaluation: {
      eval_id: 'eval-200',
      class: 'pass',
      reason: 'in_scope',
      rule: {
        consumer: 'message_policy',
        rule_id: 'pol-2',
        revision: 1,
        dialect: 'system_one',
      },
      source_eval_id: 'eval-primary-0001',
      cache_hit: true,
      instance: 'gemini-main',
      configured_model: 'gemini-decision-1',
      resolved_model: 'gemini-decision-1',
      elapsed_ms: 118,
      usage: {
        input_tokens: 42,
        output_tokens: 8,
        total_tokens: 50,
        requests: 1,
      },
    },
  },
];

describe('scion-page-project-message-policies', () => {
  beforeEach(() => {
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    window.history.replaceState({}, '', '/projects/proj-1/message-policies');
  });

  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('renders 404 and makes zero API calls when hub.message_policies is off (even if hub.system_one is on)', async () => {
    setServerFlags({ 'hub.message_policies': false, 'hub.system_one': true });
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('scion-page-404')).toBeTruthy();
    });

    expect(el.shadowRoot?.querySelector('sl-tab-group')).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders policy list, verbatim coverage banner, and hides System One columns/fields when hub.system_one is off', async () => {
    setServerFlags({ 'hub.message_policies': true, 'hub.system_one': false });

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/message-policies/decisions')) {
          return Promise.resolve(
            jsonResponse({ decisions: SAMPLE_DECISIONS_PAGE_1, total_count: 2 })
          );
        }
        if (url.includes('/message-policies/pol-2')) {
          return Promise.resolve(
            jsonResponse({ ...SAMPLE_POLICIES[1], coverage: SAMPLE_COVERAGE })
          );
        }
        if (url.includes('/message-policies')) {
          return Promise.resolve(
            jsonResponse({
              policies: SAMPLE_POLICIES,
              coverage: SAMPLE_COVERAGE,
              total_count: 2,
            })
          );
        }
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [{ id: 'agent-1', name: 'qa-1' }] }));
        }
        return Promise.resolve(
          jsonResponse({
            id: 'proj-1',
            name: 'Billing',
            _capabilities: { actions: ['read_message_policies', 'manage_message_policies'] },
          })
        );
      })
    );

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    let listEl: ScionMessagePolicyList | null = null;
    await vi.waitFor(() => {
      listEl = el.shadowRoot?.querySelector(
        'scion-message-policy-list'
      ) as ScionMessagePolicyList | null;
      expect(listEl?.shadowRoot?.querySelector('[data-policies-table]')).toBeTruthy();
    });

    // Coverage banner renders API values verbatim and contains no extra static claims or notes.
    const banner = listEl!.shadowRoot!.querySelector('[data-coverage-banner]')!;
    expect(banner.querySelector('[data-coverage-static]')).toBeNull();
    const enforcedChips = Array.from(
      banner.querySelectorAll('[data-coverage-enforced] [data-surface]')
    ).map((n) => n.textContent?.trim());
    expect(enforcedChips).toEqual(SAMPLE_COVERAGE.enforced_surfaces);

    const uncoveredChips = Array.from(
      banner.querySelectorAll('[data-coverage-uncovered] [data-uncovered]')
    ).map((n) => n.textContent?.trim());
    expect(uncoveredChips).toEqual(SAMPLE_COVERAGE.uncovered_agent_paths);

    const normalizedBannerText = banner.textContent?.replace(/\s+/g, ' ').trim();
    expect(normalizedBannerText).toBe(
      [
        'Coverage on this Hub',
        'Evaluated',
        SAMPLE_COVERAGE.enforced_surfaces.join(''),
        'NOT evaluated (agent)',
        SAMPLE_COVERAGE.uncovered_agent_paths.join(''),
      ].join(' ')
    );

    // hub.system_one off hides the Rule engines column and System One badges.
    expect(listEl!.shadowRoot!.querySelector('[data-col-engines]')).toBeNull();
    expect(listEl!.shadowRoot!.querySelector('[data-system-one-badge]')).toBeNull();

    // Open detail drawer for topic-guard -> System One section stays hidden while hub.system_one is off.
    const topicBtn = listEl!.shadowRoot!.querySelector(
      '[data-policy-name="topic-guard"]'
    ) as HTMLButtonElement;
    topicBtn.click();
    await vi.waitFor(() => {
      expect(listEl!.shadowRoot!.querySelector('[data-detail-violation-rule]')).toBeTruthy();
    });
    expect(listEl!.shadowRoot!.querySelector('[data-detail-system-one-section]')).toBeNull();
  });

  it('shows System One column, badges, and detail fields when hub.system_one is on', async () => {
    setServerFlags({ 'hub.message_policies': true, 'hub.system_one': true });

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/message-policies/decisions')) {
          return Promise.resolve(jsonResponse({ decisions: [], total_count: 0 }));
        }
        if (url.includes('/message-policies/pol-2')) {
          return Promise.resolve(
            jsonResponse({ ...SAMPLE_POLICIES[1], coverage: SAMPLE_COVERAGE })
          );
        }
        if (url.includes('/message-policies')) {
          return Promise.resolve(
            jsonResponse({
              policies: SAMPLE_POLICIES,
              coverage: SAMPLE_COVERAGE,
              total_count: 2,
            })
          );
        }
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [] }));
        }
        return Promise.resolve(
          jsonResponse({
            id: 'proj-1',
            name: 'Billing',
            _capabilities: { actions: ['read_message_policies', 'manage_message_policies'] },
          })
        );
      })
    );

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    let listEl: ScionMessagePolicyList | null = null;
    await vi.waitFor(() => {
      listEl = el.shadowRoot?.querySelector(
        'scion-message-policy-list'
      ) as ScionMessagePolicyList | null;
      expect(listEl?.shadowRoot?.querySelector('[data-col-engines]')).toBeTruthy();
    });

    expect(listEl!.shadowRoot!.querySelector('[data-system-one-badge]')).toBeTruthy();

    const topicBtn = listEl!.shadowRoot!.querySelector(
      '[data-policy-name="topic-guard"]'
    ) as HTMLButtonElement;
    topicBtn.click();
    await vi.waitFor(() => {
      expect(listEl!.shadowRoot!.querySelector('[data-detail-system-one-section]')).toBeTruthy();
    });
    expect(
      listEl!.shadowRoot!.querySelector('[data-detail-system-one-rule]')?.textContent
    ).toContain('system_one');
  });

  it('shows read-only indicator for viewer capability and renders no edit controls', async () => {
    setServerFlags({ 'hub.message_policies': true, 'hub.system_one': false });

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/message-policies/decisions')) {
          return Promise.resolve(jsonResponse({ decisions: [], total_count: 0 }));
        }
        if (url.includes('/message-policies')) {
          return Promise.resolve(
            jsonResponse({
              policies: SAMPLE_POLICIES,
              coverage: SAMPLE_COVERAGE,
              total_count: 2,
            })
          );
        }
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [] }));
        }
        return Promise.resolve(
          jsonResponse({
            id: 'proj-1',
            name: 'Billing',
            _capabilities: { actions: ['read_message_policies'] },
          })
        );
      })
    );

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    let listEl: ScionMessagePolicyList | null = null;
    await vi.waitFor(() => {
      listEl = el.shadowRoot?.querySelector(
        'scion-message-policy-list'
      ) as ScionMessagePolicyList | null;
      expect(listEl?.shadowRoot?.querySelector('[data-readonly-indicator]')).toBeTruthy();
    });

    expect(listEl!.shadowRoot!.textContent).not.toContain('New policy');
    expect(listEl!.shadowRoot!.textContent).not.toContain('Delete');
  });

  it('blocks access when project capabilities lack read_message_policies', async () => {
    setServerFlags({ 'hub.message_policies': true });
    const fetchMock = vi.fn().mockImplementation(() =>
      Promise.resolve(
        jsonResponse({
          id: 'proj-1',
          name: 'Billing',
          _capabilities: { actions: ['read'] },
        })
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('[data-page-forbidden]')).toBeTruthy();
    });

    // Only the project fetch ran; no message-policies endpoints were called.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('handles empty policy list (still showing coverage banner) and API errors', async () => {
    setServerFlags({ 'hub.message_policies': true });

    let failPolicies = false;
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/message-policies/decisions')) {
          return Promise.resolve(jsonResponse({ decisions: [], total_count: 0 }));
        }
        if (url.includes('/message-policies')) {
          if (failPolicies) {
            return Promise.resolve(jsonResponse({ error: { message: 'Backend timeout' } }, 500));
          }
          return Promise.resolve(
            jsonResponse({ policies: [], coverage: SAMPLE_COVERAGE, total_count: 0 })
          );
        }
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [] }));
        }
        return Promise.resolve(
          jsonResponse({
            id: 'proj-1',
            name: 'Billing',
            _capabilities: { actions: ['read_message_policies'] },
          })
        );
      })
    );

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    let listEl: ScionMessagePolicyList | null = null;
    await vi.waitFor(() => {
      listEl = el.shadowRoot?.querySelector(
        'scion-message-policy-list'
      ) as ScionMessagePolicyList | null;
      expect(listEl?.shadowRoot?.querySelector('[data-policies-empty]')).toBeTruthy();
    });
    expect(listEl!.shadowRoot!.querySelector('[data-coverage-banner]')).toBeTruthy();

    failPolicies = true;
    await listEl!.loadPolicies();
    await listEl!.updateComplete;
    const errEl = listEl!.shadowRoot!.querySelector('[data-policies-error]');
    expect(errEl?.textContent).toContain('Backend timeout');
  });

  it('supports Policy outcomes filtering, cursor paging, rejected no-message-link, and content-free evaluation drawer', async () => {
    setServerFlags({ 'hub.message_policies': true, 'hub.system_one': true });
    window.history.replaceState({}, '', '/projects/proj-1/message-policies?tab=outcomes');

    const requestedUrls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        requestedUrls.push(url);
        if (url.includes('/message-policies/decisions')) {
          if (url.includes('cursor=cur-page-2')) {
            return Promise.resolve(
              jsonResponse({
                decisions: [
                  {
                    ...SAMPLE_DECISIONS_PAGE_1[0],
                    id: 'dec-3',
                  },
                ],
                total_count: 3,
              })
            );
          }
          return Promise.resolve(
            jsonResponse({
              decisions: SAMPLE_DECISIONS_PAGE_1,
              next_cursor: 'cur-page-2',
              total_count: 3,
            })
          );
        }
        if (url.includes('/message-policies')) {
          return Promise.resolve(
            jsonResponse({
              policies: SAMPLE_POLICIES,
              coverage: SAMPLE_COVERAGE,
              total_count: 2,
            })
          );
        }
        if (url.includes('/agents')) {
          return Promise.resolve(
            jsonResponse({
              agents: [
                { id: 'agent-1', name: 'qa-1' },
                { id: 'agent-2', name: 'db-1' },
              ],
            })
          );
        }
        return Promise.resolve(
          jsonResponse({
            id: 'proj-1',
            name: 'Billing',
            _capabilities: { actions: ['read_message_policies'] },
          })
        );
      })
    );

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    let outcomesEl: ScionMessagePolicyOutcomes | null = null;
    await vi.waitFor(() => {
      outcomesEl = el.shadowRoot?.querySelector(
        'scion-message-policy-outcomes'
      ) as ScionMessagePolicyOutcomes | null;
      expect(outcomesEl?.shadowRoot?.querySelector('[data-outcomes-table]')).toBeTruthy();
    });

    // Rejected row (dec-1) carries NO message ref; admitted row (dec-2) shows msg ref.
    const rejectedRow = outcomesEl!.shadowRoot!.querySelector('[data-outcome-row="dec-1"]');
    expect(rejectedRow?.querySelector('[data-outcome-message-ref]')).toBeNull();
    const admittedRow = outcomesEl!.shadowRoot!.querySelector('[data-outcome-row="dec-2"]');
    expect(admittedRow?.querySelector('[data-outcome-message-ref="msg-admitted-99"]')).toBeTruthy();

    // Open row drawer on dec-2 -> renders content-free evaluation projection.
    const openBtn = outcomesEl!.shadowRoot!.querySelector(
      '[data-outcome-open="dec-2"]'
    ) as HTMLButtonElement;
    openBtn.click();
    await outcomesEl!.updateComplete;
    const proj = outcomesEl!.shadowRoot!.querySelector('[data-evaluation-projection]');
    expect(proj).toBeTruthy();
    expect(proj?.querySelector('[data-eval-rule-ref]')?.textContent).toContain('pol-2');
    expect(proj?.querySelector('[data-eval-source-id]')?.textContent?.trim()).toBe(
      'eval-primary-0001'
    );
    expect(proj?.querySelector('[data-eval-usage]')?.textContent).toContain('50');

    // Cursor paging: click Next -> sends cursor=cur-page-2, then renders page 2.
    const nextBtn = outcomesEl!.shadowRoot!.querySelector('[data-outcomes-next]') as HTMLElement;
    nextBtn.click();
    await vi.waitFor(() => {
      expect(
        requestedUrls.some(
          (u) => u.includes('/message-policies/decisions') && u.includes('cursor=cur-page-2')
        )
      ).toBe(true);
      expect(outcomesEl!.shadowRoot!.querySelector('[data-outcome-row="dec-3"]')).toBeTruthy();
    });

    // Apply mode filter -> resets cursor and passes mode=enforce.
    const modeSel = outcomesEl!.shadowRoot!.querySelector(
      '[data-outcome-filter-mode]'
    ) as HTMLSelectElement;
    modeSel.value = 'enforce';
    modeSel.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await vi.waitFor(() => {
      expect(
        requestedUrls.some(
          (u) =>
            u.includes('/message-policies/decisions') &&
            u.includes('mode=enforce') &&
            !u.includes('cursor=')
        )
      ).toBe(true);
    });
  });

  it('shows the Message Policies link on project-detail only when flag is on AND caller can read', async () => {
    window.history.replaceState({}, '', '/projects/proj-1');
    class MockEventSource {
      addEventListener(): void {}
      removeEventListener(): void {}
      close(): void {}
    }
    vi.stubGlobal('EventSource', MockEventSource);

    let projectCaps = { actions: ['read'] };
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [], totalCount: 0 }));
        }
        return Promise.resolve(
          jsonResponse({ id: 'proj-1', name: 'Alpha Project', _capabilities: projectCaps })
        );
      })
    );

    // Flag OFF + read capability -> hidden.
    projectCaps = { actions: ['read', 'read_message_policies'] };
    setServerFlags({ 'hub.message_policies': false });
    const detail = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    document.body.appendChild(detail);
    await vi.waitFor(() => {
      expect(detail.shadowRoot?.textContent).toContain('Alpha Project');
    });
    expect(detail.shadowRoot?.querySelector('[data-message-policies-button]')).toBeNull();

    // Flag ON + read capability -> visible with correct href.
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    setServerFlags({ 'hub.message_policies': true });
    await detail.updateComplete;

    const link = detail.shadowRoot?.querySelector(
      '[data-message-policies-link]'
    ) as HTMLAnchorElement | null;
    expect(link?.getAttribute('href')).toBe('/projects/proj-1/message-policies');

    // Flag ON + NO read_message_policies capability -> hidden.
    detail.remove();
    projectCaps = { actions: ['read'] };
    const detailNoPerm = document.createElement(
      'scion-page-project-detail'
    ) as ScionPageProjectDetail;
    document.body.appendChild(detailNoPerm);
    await vi.waitFor(() => {
      expect(detailNoPerm.shadowRoot?.textContent).toContain('Alpha Project');
    });
    expect(detailNoPerm.shadowRoot?.querySelector('[data-message-policies-button]')).toBeNull();
  });

  it('resolves app-shell page title to Message Policies only when hub.message_policies is on', async () => {
    const shell = document.createElement('scion-app') as ScionAppShell;
    shell.currentPath = '/projects/proj-1/message-policies';

    setServerFlags({ 'hub.message_policies': false });
    document.body.appendChild(shell);
    await shell.updateComplete;
    expect((shell as unknown as { getPageTitle(): string }).getPageTitle()).toBe('Page Not Found');

    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    setServerFlags({ 'hub.message_policies': true });
    expect((shell as unknown as { getPageTitle(): string }).getPageTitle()).toBe(
      'Message Policies'
    );
  });

  it('renders mutated coverage fixture verbatim with one enforced surface and empty uncovered list (R1)', async () => {
    setServerFlags({ 'hub.message_policies': true, 'hub.system_one': false });
    const mutatedCoverage = {
      enforced_surfaces: ['agent_to_user'],
      uncovered_agent_paths: [] as string[],
    };

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/message-policies/decisions')) {
          return Promise.resolve(jsonResponse({ decisions: [], total_count: 0 }));
        }
        if (url.includes('/message-policies')) {
          return Promise.resolve(
            jsonResponse({ policies: [], coverage: mutatedCoverage, total_count: 0 })
          );
        }
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [] }));
        }
        return Promise.resolve(
          jsonResponse({
            id: 'proj-1',
            name: 'Billing',
            _capabilities: { actions: ['read_message_policies'] },
          })
        );
      })
    );

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    let listEl: ScionMessagePolicyList | null = null;
    await vi.waitFor(() => {
      listEl = el.shadowRoot?.querySelector(
        'scion-message-policy-list'
      ) as ScionMessagePolicyList | null;
      expect(listEl?.shadowRoot?.querySelector('[data-coverage-banner]')).toBeTruthy();
    });

    const banner = listEl!.shadowRoot!.querySelector('[data-coverage-banner]')!;
    expect(banner.textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'Coverage on this Hub Evaluated agent_to_user NOT evaluated (agent) None'
    );
  });

  it('ignores stale out-of-order responses in loadDecisions and loadPolicies (O1)', async () => {
    setServerFlags({ 'hub.message_policies': true, 'hub.system_one': false });
    window.history.replaceState({}, '', '/projects/proj-1/message-policies?tab=outcomes');

    let resolveFirstDecisions!: (res: Response) => void;
    let resolveSecondDecisions!: (res: Response) => void;
    let decisionCall = 0;

    let resolveFirstPolicies!: (res: Response) => void;
    let resolveSecondPolicies!: (res: Response) => void;
    let policyCall = 0;

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/message-policies/decisions')) {
          decisionCall += 1;
          if (decisionCall === 1) {
            return new Promise<Response>((resolve) => {
              resolveFirstDecisions = resolve;
            });
          }
          return new Promise<Response>((resolve) => {
            resolveSecondDecisions = resolve;
          });
        }
        if (url.includes('/message-policies')) {
          policyCall += 1;
          if (policyCall === 1) {
            return new Promise<Response>((resolve) => {
              resolveFirstPolicies = resolve;
            });
          }
          return new Promise<Response>((resolve) => {
            resolveSecondPolicies = resolve;
          });
        }
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [] }));
        }
        return Promise.resolve(
          jsonResponse({
            id: 'proj-1',
            name: 'Billing',
            _capabilities: { actions: ['read_message_policies'] },
          })
        );
      })
    );

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    let outcomesEl: ScionMessagePolicyOutcomes | null = null;
    let listEl: ScionMessagePolicyList | null = null;
    await vi.waitFor(() => {
      outcomesEl = el.shadowRoot?.querySelector(
        'scion-message-policy-outcomes'
      ) as ScionMessagePolicyOutcomes | null;
      listEl = el.shadowRoot?.querySelector(
        'scion-message-policy-list'
      ) as ScionMessagePolicyList | null;
      expect(decisionCall).toBe(1);
      expect(policyCall).toBe(1);
    });

    // Trigger second requests before first requests resolve.
    const modeSel = outcomesEl!.shadowRoot!.querySelector(
      '[data-outcome-filter-mode]'
    ) as HTMLSelectElement;
    modeSel.value = 'enforce';
    modeSel.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));

    const policyModeSel = listEl!.shadowRoot!.querySelector(
      '[data-filter-mode]'
    ) as HTMLSelectElement;
    policyModeSel.value = 'enforce';
    policyModeSel.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));

    await vi.waitFor(() => {
      expect(decisionCall).toBe(2);
      expect(policyCall).toBe(2);
    });

    // Resolve SECOND (newer) responses first.
    resolveSecondDecisions(
      jsonResponse({
        decisions: [{ ...SAMPLE_DECISIONS_PAGE_1[0], id: 'dec-newer' }],
        total_count: 1,
      })
    );
    resolveSecondPolicies(
      jsonResponse({
        policies: [{ ...SAMPLE_POLICIES[0], id: 'pol-newer', name: 'newer-policy' }],
        coverage: SAMPLE_COVERAGE,
        total_count: 1,
      })
    );

    await vi.waitFor(() => {
      expect(outcomesEl!.shadowRoot!.querySelector('[data-outcome-row="dec-newer"]')).toBeTruthy();
      expect(listEl!.shadowRoot!.querySelector('[data-policy-row="pol-newer"]')).toBeTruthy();
    });

    // Now resolve FIRST (stale) responses -> must be ignored.
    resolveFirstDecisions(
      jsonResponse({
        decisions: [{ ...SAMPLE_DECISIONS_PAGE_1[0], id: 'dec-stale' }],
        total_count: 1,
      })
    );
    resolveFirstPolicies(
      jsonResponse({
        policies: [{ ...SAMPLE_POLICIES[0], id: 'pol-stale', name: 'stale-policy' }],
        coverage: SAMPLE_COVERAGE,
        total_count: 1,
      })
    );

    await new Promise((r) => setTimeout(r, 20));
    await outcomesEl!.updateComplete;
    await listEl!.updateComplete;

    expect(outcomesEl!.shadowRoot!.querySelector('[data-outcome-row="dec-newer"]')).toBeTruthy();
    expect(outcomesEl!.shadowRoot!.querySelector('[data-outcome-row="dec-stale"]')).toBeNull();
    expect(listEl!.shadowRoot!.querySelector('[data-policy-row="pol-newer"]')).toBeTruthy();
    expect(listEl!.shadowRoot!.querySelector('[data-policy-row="pol-stale"]')).toBeNull();
  });

  it('retains active policyFilter option, shows shadow action verbatim, hides System One drawer when off, suppresses message_id on rejected rows, and never renders extra disclosure keys (O2/O3/O4)', async () => {
    setServerFlags({ 'hub.message_policies': true, 'hub.system_one': false });
    window.history.replaceState(
      {},
      '',
      '/projects/proj-1/message-policies?tab=outcomes&policy=pol-deleted-999'
    );

    const decisionsFixture = [
      {
        ...SAMPLE_DECISIONS_PAGE_1[0],
        id: 'dec-rej-with-msg',
        action: 'rejected',
        message_id: 'msg-should-never-render',
      },
      {
        ...SAMPLE_DECISIONS_PAGE_1[1],
        id: 'dec-shadow-viol',
        mode: 'shadow',
        class: 'violation',
        action: 'shadow',
        evaluation: {
          ...SAMPLE_DECISIONS_PAGE_1[1].evaluation,
          body: 'SECRET_BODY_TEXT',
          prompt: 'SECRET_PROMPT_TEXT',
          copy_facts: { leak: 'SECRET_COPY_FACT' },
          answers: {
            on_topic: {
              question_id: 'on_topic',
              primitive: 'choice',
              status: 'answered',
              required: true,
              choice: 'out_of_scope',
              notes: ['SECRET_NOTE_TEXT'],
            },
          },
        },
      },
    ];

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/message-policies/decisions')) {
          return Promise.resolve(jsonResponse({ decisions: decisionsFixture, total_count: 2 }));
        }
        if (url.includes('/message-policies')) {
          return Promise.resolve(
            jsonResponse({
              policies: SAMPLE_POLICIES,
              coverage: SAMPLE_COVERAGE,
              total_count: 2,
            })
          );
        }
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [] }));
        }
        return Promise.resolve(
          jsonResponse({
            id: 'proj-1',
            name: 'Billing',
            _capabilities: { actions: ['read_message_policies'] },
          })
        );
      })
    );

    const el = document.createElement(
      'scion-page-project-message-policies'
    ) as ScionPageProjectMessagePolicies;
    document.body.appendChild(el);

    let outcomesEl: ScionMessagePolicyOutcomes | null = null;
    await vi.waitFor(() => {
      outcomesEl = el.shadowRoot?.querySelector(
        'scion-message-policy-outcomes'
      ) as ScionMessagePolicyOutcomes | null;
      expect(outcomesEl?.shadowRoot?.querySelector('[data-outcomes-table]')).toBeTruthy();
    });

    // O2: deep-linked policy ID not in current policies list still has a selectable option.
    expect(
      outcomesEl!.shadowRoot!.querySelector(
        '[data-outcome-filter-policy] sl-option[value="pol-deleted-999"][data-active-policy-fallback]'
      )
    ).toBeTruthy();

    // O4: rejected decision WITH a message_id fixture renders NO message link in row or drawer.
    const rejRow = outcomesEl!.shadowRoot!.querySelector('[data-outcome-row="dec-rej-with-msg"]')!;
    expect(rejRow.querySelector('[data-outcome-message-ref]')).toBeNull();
    (rejRow.querySelector('[data-outcome-open="dec-rej-with-msg"]') as HTMLButtonElement).click();
    await outcomesEl!.updateComplete;
    expect(outcomesEl!.shadowRoot!.querySelector('[data-drawer-message-id]')).toBeNull();

    // O3: shadow row with class=violation renders action verbatim ("shadow"), not "would reject".
    const shadowRow = outcomesEl!.shadowRoot!.querySelector(
      '[data-outcome-row="dec-shadow-viol"]'
    )!;
    expect(shadowRow.textContent).not.toContain('would reject');
    expect(shadowRow.textContent).toContain('shadow');

    // O4: opening outcome drawer with hub.system_one OFF hides the System One projection section.
    (shadowRow.querySelector('[data-outcome-open="dec-shadow-viol"]') as HTMLButtonElement).click();
    await outcomesEl!.updateComplete;
    expect(outcomesEl!.shadowRoot!.querySelector('[data-evaluation-projection]')).toBeNull();

    // O4 disclosure regression: flip hub.system_one ON -> projection renders, but extra/unknown
    // keys (body, prompt, copy_facts, answers[*].notes) are NEVER rendered.
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    setServerFlags({ 'hub.message_policies': true, 'hub.system_one': true });
    await el.updateComplete;
    await outcomesEl!.updateComplete;

    const drawer = outcomesEl!.shadowRoot!.querySelector('[data-outcome-drawer]')!;
    expect(drawer.querySelector('[data-evaluation-projection]')).toBeTruthy();
    expect(drawer.textContent).not.toContain('SECRET_BODY_TEXT');
    expect(drawer.textContent).not.toContain('SECRET_PROMPT_TEXT');
    expect(drawer.textContent).not.toContain('SECRET_COPY_FACT');
    expect(drawer.textContent).not.toContain('SECRET_NOTE_TEXT');
  });
});

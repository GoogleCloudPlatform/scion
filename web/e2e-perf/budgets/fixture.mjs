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
 * Deterministic API responses for the web counter budgets: 100 agents in
 * one project, seen by a non-admin project member, in the shape of
 * pkg/hub/perf_budget_test.go's seeded fixture (itself the perf/bench seed
 * shape: labels, activity, ancestry, a small appliedConfig on 90% of
 * agents and a several-KB pre-start hook script on 10%).
 *
 * Every value is a function of the agent index: no clock, no random IDs,
 * so the rendered DOM is the same on every run.
 *
 * Field names must match the hub's real responses. fixture-schema.json
 * holds the hub's field names per request, written by the Go test;
 * fixture.test.mjs fails when buildFixture() does not produce exactly
 * those fields. See docs-site/src/content/docs/contributing/perf-tracing.md.
 */

export const AGENT_COUNT = 100;
export const PROJECT_ID = '0b5e7a1c-0000-4000-8000-000000000001';
const OWNER_ID = '0b5e7a1c-0000-4000-8000-0000000000a1'; // alice, project owner
const MEMBER_ID = '0b5e7a1c-0000-4000-8000-0000000000c1'; // carol, the caller
const BASE_MS = Date.parse('2026-10-01T12:00:00Z');
const ZERO_TIME = '0001-01-01T00:00:00Z';
const PHASES = ['running', 'running', 'running', 'stopped', 'error', 'created'];
const ACTIVITIES = ['idle', 'thinking', 'executing', 'waiting_for_input'];
const OWNER_ACTIONS = [
  'read',
  'update',
  'delete',
  'attach',
  'lifecycle',
  'port_access',
  'set_message_mode',
  'grant_hub_mode',
];

const iso = (ms) => new Date(ms).toISOString().replace('.000Z', 'Z');
const agentId = (i) => `0b5e7a1c-0000-4000-8000-${String(1000 + i).padStart(12, '0')}`;

const PROJECT = {
  _capabilities: { actions: ['read'] },
  agentCount: AGENT_COUNT,
  created: '2026-10-01T12:00:00Z',
  createdBy: OWNER_ID,
  crossProjectInbound: 'none',
  crossProjectInboundRevision: 1,
  id: PROJECT_ID,
  name: 'Demo Project',
  ownerId: OWNER_ID,
  ownerName: 'Alice',
  projectType: 'hub-managed',
  slug: 'demo-project',
  updated: '2026-10-01T12:00:00Z',
};

function hookScript(i) {
  const lines = ['#!/usr/bin/env bash', 'set -euo pipefail'];
  for (let j = 0; j < 80 + (i % 40); j++) {
    const n = (BigInt(i + 1) * 1000003n + BigInt(j) * 998244353n) * 1234567891n;
    lines.push(`echo 'bench pre-start step ${j}: ${n}'`);
  }
  return lines.join('\n') + '\n';
}

/** One agent, as GET .../agents returns it to the member. */
function agent(i) {
  const id = agentId(i);
  const phase = PHASES[i % PHASES.length];
  // About 40% are the caller's own agents: full actions and a visible env.
  const own = i % 5 === 1 || i % 5 === 3;
  const creator = own ? MEMBER_ID : OWNER_ID;
  const large = i % 10 === 0;
  const updated = iso(BASE_MS + i * 1000);
  const appliedConfig = {
    agentRole: 'baseline',
    creatorName: 'bench-owner@example.test',
    harnessConfig: 'default-claude',
    image: 'ghcr.io/scion-project/claude-harness:latest',
    model: 'claude-sonnet-5',
    profile: 'default',
    templateHash: 'sha256:' + 'ab'.repeat(32),
    templateId: 'tmpl-bench-default',
  };
  if (own) {
    appliedConfig.env = {
      SCION_AGENT_SEQ: String(i),
      SCION_BENCH_RUN: '1',
      SCION_LOG_LEVEL: 'info',
      SCION_PROJECT: 'demo-project',
    };
  }
  if (large) {
    appliedConfig.projectPreStartHookId = `0b5e7a1c-0000-4000-8000-${String(5000 + i).padStart(12, '0')}`;
    appliedConfig.projectPreStartHookScript = hookScript(i);
  }
  const a = {
    _capabilities: { actions: own ? OWNER_ACTIONS : ['read'] },
    activity: ACTIVITIES[i % ACTIVITIES.length],
    appliedConfig,
    created: iso(BASE_MS - 3600_000 + i * 1000),
    createdBy: creator,
    deletedAt: ZERO_TIME,
    deletion: null,
    detached: false,
    generation: 1,
    harnessConfig: 'default-claude',
    id,
    labels: { bench: 'true', 'bench-seq': String(i), 'bench-tier': large ? 'large' : 'small' },
    lastActivityEvent: ZERO_TIME,
    lastSeen: phase === 'created' ? ZERO_TIME : updated,
    messageMode: 'project',
    name: `Bench Agent ${i}`,
    ownerId: creator,
    phase,
    project: 'Demo Project',
    projectId: PROJECT_ID,
    provisionedOnly: false,
    slug: `bench-agent-${String(i).padStart(5, '0')}`,
    startedAt: ZERO_TIME,
    stateVersion: 1,
    template: 'default',
    updated,
  };
  // 60% no ancestry, 25% created by a user, 15% by an earlier agent.
  const r = i % 20;
  if (r >= 12 && r < 17) a.ancestry = [creator];
  else if (r >= 17) a.ancestry = [creator, agentId(i - 1)];
  return a;
}

/**
 * buildFixture returns { projectId, agents, me, responses }, where
 * responses maps each request path (pathname plus query, as the page
 * sends it) to { status, body }.
 */
export function buildFixture() {
  // Newest first: the order of sort=updated&dir=desc.
  const agents = [];
  for (let i = AGENT_COUNT - 1; i >= 0; i--) agents.push(agent(i));
  const listCaps = { actions: ['create', 'list'] };
  const p = `/api/v1/projects/${PROJECT_ID}`;
  const responses = {
    '/api/v1/settings/public': {
      status: 200,
      body: {
        agentSecretsUserScopeOnly: false,
        autoExposePortsEnabled: false,
        nativeChatEnabled: true,
        telemetryEnabled: false,
      },
    },
    '/api/v1/experiments': {
      status: 200,
      body: {
        experiments: {
          'hub.artifacts': false,
          'web.chat_scheduled_send': false,
          'web.gcs_links': false,
          'web.terminal_workspace': true,
        },
      },
    },
    '/api/v1/auth/admin-status': {
      status: 200,
      body: {
        isAdmin: true,
        isSuperAdmin: false,
        permissions: [
          'broker.create',
          'broker.list',
          'broker.read',
          'gcp_service_account.list',
          'gcp_service_account.read',
          'group.list',
          'group.read',
          'harness_config.list',
          'harness_config.read',
          'hub.settings.read',
          'inbox.read',
          'inbox.write',
          'project.create',
          'quota.read',
          'role.read',
          'skill.list',
          'skill.read',
          'template.list',
          'template.read',
          'user.list',
          'user.read',
          'user_skill_injection.update',
        ],
      },
    },
    '/api/v1/system/status': { status: 404, body: '404 page not found\n' },
    '/api/v1/chat/spaces': { status: 200, body: { spaces: [] } },
    '/api/v1/chat/dms': { status: 200, body: { dms: [] } },
    '/api/v1/messages?unread=true': { status: 200, body: { items: [] } },
    '/api/v1/notifications?acknowledged=false': { status: 200, body: [] },
    '/api/v1/projects?limit=1': {
      status: 200,
      body: { _capabilities: { actions: ['create'] }, projects: [PROJECT], totalCount: 1 },
    },
    [p]: { status: 200, body: PROJECT },
    [`${p}/metrics-summary`]: { status: 200, body: { available: false } },
    [`${p}/metrics/summary`]: {
      status: 200,
      body: {
        activeAgents: 0,
        mostUsedModels: [],
        mostUsedTools: [],
        projectId: PROJECT_ID,
        totalSessions: 0,
        totalTokensCached: 0,
        totalTokensInput: 0,
        totalTokensOutput: 0,
        totalTokensReasoning: 0,
      },
    },
    [`${p}/workspace/files?limit=500`]: {
      status: 200,
      body: { files: [], totalCount: 0, totalSize: 0 },
    },
    [`${p}/agents?sort=updated&dir=desc&limit=25&fit=50&stats=1`]: {
      status: 200,
      body: {
        _capabilities: listCaps,
        agents: agents.slice(0, 25),
        complete: false,
        dir: 'desc',
        nextCursor: 'djIsdXBkYXRlZCxkZXNjLGZpeHR1cmU=',
        serverTime: '2026-10-01T12:05:00Z',
        sort: 'updated',
        stats: {
          agents: agents.map((a) => [a.id, a.phase]),
          running: agents.filter((a) => a.phase === 'running').length,
          total: AGENT_COUNT,
        },
        totalCount: AGENT_COUNT,
      },
    },
    [`${p}/agents?limit=500`]: {
      status: 200,
      body: {
        _capabilities: listCaps,
        agents,
        serverTime: '2026-10-01T12:05:00Z',
        totalCount: AGENT_COUNT,
      },
    },
  };
  return {
    projectId: PROJECT_ID,
    agents: AGENT_COUNT,
    me: { id: MEMBER_ID, email: 'carol@test.com', displayName: 'Carol', role: 'member' },
    responses,
  };
}

/**
 * fieldPaths returns the sorted, de-duplicated field paths of a JSON
 * value: object keys joined with '.', array elements as '[]'. It matches
 * perfBudgetFieldPaths in pkg/hub/perf_budget_test.go.
 */
export function fieldPaths(v) {
  const set = new Set();
  const walk = (x, prefix) => {
    if (Array.isArray(x)) {
      for (const c of x) walk(c, prefix + '[]');
    } else if (x !== null && typeof x === 'object') {
      for (const [k, c] of Object.entries(x)) {
        const p = prefix ? `${prefix}.${k}` : k;
        set.add(p);
        walk(c, p);
      }
    }
  };
  walk(v, '');
  // Byte-wise order, as Go's sort.Strings.
  return [...set].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
}

/**
 * schemaOf computes fixture-schema.json's structure for a fixture: the
 * project ID in each path becomes '{project}'; a string body has no fields.
 */
export function schemaOf(fx) {
  const endpoints = {};
  for (const [path, rec] of Object.entries(fx.responses)) {
    endpoints[path.split(fx.projectId).join('{project}')] = {
      status: rec.status,
      fields: typeof rec.body === 'string' ? [] : fieldPaths(rec.body),
    };
  }
  endpoints['/auth/me'] = { status: 200, fields: fieldPaths(fx.me) };
  return { agents: fx.agents, endpoints };
}

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
 * ptone/scion#2493 test-plan item 9: the configure page must send an
 * explicit empty value for a field it owns (renders and lets the user clear
 * outright) when the user clears it, so the hub's recordExplicitEdits
 * (Option C) can tell "present and cleared" apart from "never touched" and
 * record the clear as an explicit CreateInputs edit — see
 * pkg/hub/applied_config_explicit_edits.go and buildConfig in
 * agent-configure.ts.
 *
 * Fields with hub-side "empty means unchanged, not cleared" semantics
 * (model, image, auth_selectedType) and fields this page never edits (task
 * is explicitly excluded from CreateInputs tracking; volumes/skills/
 * mcp_servers are not rendered here at all) are deliberately left on the
 * truthy-only guard and must keep being omitted when empty.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

interface ScionConfigPayload {
  image?: string;
  model?: string;
  user?: string;
  auth_selectedType?: string;
  task?: string;
  system_prompt?: string;
  agent_instructions?: string;
  branch?: string;
  max_turns?: number;
  max_model_calls?: number;
  max_duration?: string;
}

/** The private form-state fields and method buildConfig touches. */
interface ConfigurePrivate {
  containerUser: string;
  systemPrompt: string;
  agentInstructions: string;
  branch: string;
  maxTurns: number;
  maxModelCalls: number;
  maxDuration: string;
  modelSelection: string;
  customModelId: string;
  image: string;
  task: string;
  authMethod: string;
  buildConfig(): ScionConfigPayload;
}

function stubFetch(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(() =>
      Promise.resolve({
        ok: false,
        status: 404,
        json: async () => ({}),
        text: async () => 'not found',
      } as Response)
    )
  );
}

beforeAll(async () => {
  // Pays the dynamic-import/compile cost once, up front, instead of on
  // whichever test happens to run first -- that test was otherwise prone to
  // tripping the per-test timeout on a slow CI runner.
  await import('./agent-configure.js');
});

beforeEach(() => {
  stubFetch();
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

async function mountAgentConfigure(): Promise<ConfigurePrivate> {
  await import('./agent-configure.js');
  const el = document.createElement('scion-page-agent-configure');
  document.body.appendChild(el);
  await new Promise((r) => setTimeout(r, 0));
  await (el as HTMLElement & { updateComplete: Promise<unknown> }).updateComplete;
  // loadAgent's fetch fails (stubbed 404 above), so it never overwrites the
  // form-state fields via populateForm -- the test sets them directly below,
  // against the component's own post-mount defaults.
  return el as unknown as ConfigurePrivate;
}

describe('agent-configure buildConfig — owned fields send explicit empty values when cleared', () => {
  it('sends an explicit empty string for a cleared system prompt', async () => {
    const c = await mountAgentConfigure();
    c.systemPrompt = '';
    expect(c.buildConfig()).toHaveProperty('system_prompt', '');
  });

  it('sends an explicit empty string for a cleared agent instructions field', async () => {
    const c = await mountAgentConfigure();
    c.agentInstructions = '';
    expect(c.buildConfig()).toHaveProperty('agent_instructions', '');
  });

  it('sends an explicit empty string for a cleared container user', async () => {
    const c = await mountAgentConfigure();
    c.containerUser = '';
    expect(c.buildConfig()).toHaveProperty('user', '');
  });

  it('sends an explicit empty string for a cleared branch', async () => {
    const c = await mountAgentConfigure();
    c.branch = '';
    expect(c.buildConfig()).toHaveProperty('branch', '');
  });

  it('sends explicit zero/empty values for cleared limit fields', async () => {
    const c = await mountAgentConfigure();
    c.maxTurns = 0;
    c.maxModelCalls = 0;
    c.maxDuration = '';
    const config = c.buildConfig();
    expect(config).toHaveProperty('max_turns', 0);
    expect(config).toHaveProperty('max_model_calls', 0);
    expect(config).toHaveProperty('max_duration', '');
  });

  it('still populates owned fields with their current (non-empty) value', async () => {
    const c = await mountAgentConfigure();
    c.systemPrompt = 'be helpful';
    c.agentInstructions = 'follow the style guide';
    c.containerUser = 'agent';
    c.branch = 'feature/x';
    c.maxTurns = 5;
    const config = c.buildConfig();
    expect(config.system_prompt).toBe('be helpful');
    expect(config.agent_instructions).toBe('follow the style guide');
    expect(config.user).toBe('agent');
    expect(config.branch).toBe('feature/x');
    expect(config.max_turns).toBe(5);
  });

  it('still omits model/image/task/auth_selectedType when empty (hub-side "empty means unchanged")', async () => {
    const c = await mountAgentConfigure();
    c.modelSelection = '';
    c.customModelId = '';
    c.image = '';
    c.task = '';
    c.authMethod = '';
    const config = c.buildConfig();
    expect(config).not.toHaveProperty('model');
    expect(config).not.toHaveProperty('image');
    expect(config).not.toHaveProperty('task');
    expect(config).not.toHaveProperty('auth_selectedType');
  });
});

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
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

// Shared golden fixture (ptone/scion#2493 R2-2): the Go hub test
// (pkg/hub/applied_config_explicit_edits_test.go) loads the SAME file as its
// PATCH body, so the two cannot silently drift apart -- a future buildConfig
// change that stops matching this file breaks this test, not a hand-copied
// one in Go that nobody remembers to update.
const GOLDEN_UNTOUCHED_BODY_PATH = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../../../pkg/hub/testdata/configure-untouched-body.json'
);
const goldenUntouchedBody: Record<string, unknown> = JSON.parse(
  readFileSync(GOLDEN_UNTOUCHED_BODY_PATH, 'utf-8')
);

// Shared golden fixture: the body buildConfig emits when the user adds one
// custom env row on an agent whose AppliedConfig.Env has an unrelated template
// key and whose auto-expose control is untouched, so no auto-expose key is
// sent. Also loaded by the hub's PATCH tests (pkg/hub/auto_expose_patch_test.go),
// so the two cannot drift apart.
const GOLDEN_ROW_EDIT_BODY_PATH = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../../../pkg/hub/testdata/configure-row-edit-body.json'
);
const goldenRowEditBody: Record<string, unknown> = JSON.parse(
  readFileSync(GOLDEN_ROW_EDIT_BODY_PATH, 'utf-8')
);

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

interface ScionConfigPayloadFull extends ScionConfigPayload {
  env?: Record<string, string>;
  telemetry?: { enabled?: boolean };
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
  telemetryEnabled: boolean;
  autoExposePortsEnabled: boolean;
  buildConfig(): ScionConfigPayloadFull;
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

/**
 * Stubs fetch so loadAgent's round trip succeeds and populateForm runs
 * against a realistic, already-explicit live config: hub telemetry stamped
 * on (as resolveDerivedConfig would at create), and one explicit env key.
 * This is the shape R1-1 reproduced against -- a live config that already
 * has real values the user never typed on this visit.
 *
 * appliedConfig lets a test override the agent's appliedConfig entirely
 * (e.g. to put a key only in ac.env, not ic.env -- the live/InlineConfig
 * mismatch ptone/scion#2493 R2-1 facet (b) is about).
 */
function stubFetchWithLoadedAgent(appliedConfig?: Record<string, unknown>): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      if (url.includes('/settings/public')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ telemetryEnabled: false, autoExposePortsEnabled: false }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({
          id: 'agent-1',
          name: 'agent-1',
          projectId: 'project-1',
          phase: 'created',
          appliedConfig: appliedConfig ?? {
            model: 'claude-opus',
            inlineConfig: {
              env: { EXPLICIT_KEY: 'explicit-value' },
              telemetry: { enabled: true },
            },
          },
        }),
      } as Response);
    })
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

/**
 * Mounts the page against stubFetchWithLoadedAgent and waits for loadAgent's
 * two awaited fetches plus populateForm to finish, so the returned element's
 * form state (and the loaded* snapshots) reflect the live config exactly as
 * a real page load would -- not the component's bare post-mount defaults.
 */
async function mountAgentConfigureWithLoadedAgent(
  appliedConfig?: Record<string, unknown>
): Promise<ConfigurePrivate> {
  stubFetchWithLoadedAgent(appliedConfig);
  await import('./agent-configure.js');
  const el = document.createElement('scion-page-agent-configure');
  document.body.appendChild(el);
  const c = el as unknown as ConfigurePrivate & {
    loading: boolean;
    updateComplete: Promise<unknown>;
  };
  const deadline = Date.now() + 2000;
  while (c.loading && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    await c.updateComplete;
  }
  return c;
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

describe('agent-configure buildConfig — R1-1: untouched telemetry/auto-expose controls are never echoed', () => {
  it('sends no telemetry and no env at all for a fully untouched form loaded from a live config with hub telemetry', async () => {
    const c = await mountAgentConfigureWithLoadedAgent();
    // Sanity check: populateForm actually loaded the live telemetry value,
    // it was not left at the component's bare default.
    expect(c.telemetryEnabled).toBe(true);

    const config = c.buildConfig();
    expect(config).not.toHaveProperty('telemetry');
    // Nothing about env changed either (no custom row edited, auto-expose
    // untouched), so the whole `env` key is omitted.
    expect(config).not.toHaveProperty('env');
  });

  it('sends telemetry only after the user actually toggles it', async () => {
    const c = await mountAgentConfigureWithLoadedAgent();
    expect(c.telemetryEnabled).toBe(true);
    c.telemetryEnabled = false;
    const config = c.buildConfig();
    expect(config).toHaveProperty('telemetry');
    expect(config.telemetry?.enabled).toBe(false);
  });
});

describe('agent-configure buildConfig — R2-2: untouched-form body matches the shared golden fixture', () => {
  it('produces exactly pkg/hub/testdata/configure-untouched-body.json for an untouched, fully-loaded form', async () => {
    // No image/auth/task/harnessConfig, no custom env, no telemetry: every
    // field this scenario doesn't set is either absent (hub-side "empty
    // means unchanged" fields) or explicit-empty (owned fields) in the
    // output, and model/thinking_level/branch/user/agent_instructions/
    // system_prompt/max_turns/max_model_calls/max_duration are all present
    // -- the exact shape pkg/hub's TestApplyAgentUpdate_
    // UntouchedSaveLeavesHubTelemetryAndEnvAlone PATCHes with, loaded from
    // the SAME file.
    const c = await mountAgentConfigureWithLoadedAgent({ model: 'golden-model' });
    const config = c.buildConfig();
    expect(config).toEqual(goldenUntouchedBody);
  });
});

describe('effectiveAutoExposePorts', () => {
  it('labels a value in the explicit record as explicit', async () => {
    const { effectiveAutoExposePorts } = await import('./agent-configure.js');
    expect(
      effectiveAutoExposePorts(
        { SCION_AUTO_EXPOSE_PORTS: 'false' },
        { SCION_AUTO_EXPOSE_PORTS: 'false' },
        true
      )
    ).toEqual({ enabled: false, source: 'explicit' });
  });

  it('labels a value only in AppliedConfig.Env as project/template', async () => {
    const { effectiveAutoExposePorts } = await import('./agent-configure.js');
    expect(effectiveAutoExposePorts({ SCION_AUTO_EXPOSE_PORTS: 'true' }, {}, false)).toEqual({
      enabled: true,
      source: 'project/template',
    });
  });

  it('falls back to the hub default when AppliedConfig.Env lacks the key, ignoring the explicit record', async () => {
    const { effectiveAutoExposePorts } = await import('./agent-configure.js');
    expect(
      effectiveAutoExposePorts({ OTHER: 'x' }, { SCION_AUTO_EXPOSE_PORTS: 'false' }, true)
    ).toEqual({ enabled: true, source: 'hub default' });
    expect(effectiveAutoExposePorts(undefined, undefined, false)).toEqual({
      enabled: false,
      source: 'hub default',
    });
  });
});

/** Text of the rendered auto-expose source label. */
async function autoExposeSourceText(c: ConfigurePrivate): Promise<string> {
  const el = c as unknown as HTMLElement & { updateComplete: Promise<unknown> };
  await el.updateComplete;
  const label = el.shadowRoot?.querySelector('[data-testid="auto-expose-source"]');
  return (label?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('agent-configure — auto-expose effective value, source label and save (ptone/scion#2562 AC7)', () => {
  it('shows a project/template-derived value with its source, and an untouched save sends no env', async () => {
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { SCION_AUTO_EXPOSE_PORTS: 'true' },
      inlineConfig: {},
    });
    expect(c.autoExposePortsEnabled).toBe(true);
    expect((c as unknown as { autoExposeSource: string }).autoExposeSource).toBe(
      'project/template'
    );
    expect(c.buildConfig()).toEqual(goldenUntouchedBody);
  });

  it('shows the hub default when no tier set the key', async () => {
    const c = await mountAgentConfigureWithLoadedAgent({ model: 'golden-model' });
    expect(c.autoExposePortsEnabled).toBe(false);
    expect((c as unknown as { autoExposeSource: string }).autoExposeSource).toBe('hub default');
  });

  it('renders the source label, switching to explicit (unsaved) once the control changes', async () => {
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { SCION_AUTO_EXPOSE_PORTS: 'false' },
      inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'false' } },
    });
    expect(await autoExposeSourceText(c)).toBe('Source: explicit');
    c.autoExposePortsEnabled = true;
    expect(await autoExposeSourceText(c)).toBe('Source: explicit (unsaved)');
  });

  it('keys the explicit label on CreateInputs when the agent has it', async () => {
    // A hub stamp in InlineConfig.Env that CreateInputs lacks is not explicit.
    const stamped = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { SCION_AUTO_EXPOSE_PORTS: 'true' },
      inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
      createInputs: { inlineConfig: {} },
    });
    expect(await autoExposeSourceText(stamped)).toBe('Source: project/template');

    const recorded = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { SCION_AUTO_EXPOSE_PORTS: 'false' },
      inlineConfig: {},
      createInputs: { inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'false' } } },
    });
    expect(await autoExposeSourceText(recorded)).toBe('Source: explicit');
  });

  it('a custom-row edit sends the rows only, never the untouched auto-expose keys', async () => {
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: {
        TEMPLATE_KEY: 'x',
        SCION_AUTO_EXPOSE_PORTS: 'true',
        SCION_AUTO_EXPOSE_MODE: 'denylist',
      },
      inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
    });
    const withEnvEntries = c as unknown as {
      envEntries: { key: string; value: string }[];
    };
    expect(withEnvEntries.envEntries).toEqual([{ key: 'TEMPLATE_KEY', value: 'x' }]);
    withEnvEntries.envEntries = [
      { key: 'TEMPLATE_KEY', value: 'x' },
      { key: 'FOO', value: 'bar' },
    ];
    // Shared with the hub's TestApplyAgentUpdate_AutoExposeUntouchedSave and
    // related PATCH tests.
    expect(c.buildConfig()).toEqual(goldenRowEditBody);
  });

  it('custom rows read AppliedConfig.Env only', async () => {
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { LIVE_KEY: 'v' },
      inlineConfig: { env: { INLINE_ONLY: 'w' } },
    });
    const rows = (c as unknown as { envEntries: { key: string; value: string }[] }).envEntries;
    expect(rows).toEqual([{ key: 'LIVE_KEY', value: 'v' }]);
  });

  it('toggling the control sends the auto-expose keys as explicit values', async () => {
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { SCION_AUTO_EXPOSE_PORTS: 'true' },
      inlineConfig: {},
    });
    c.autoExposePortsEnabled = false;
    expect(c.buildConfig().env).toEqual({ SCION_AUTO_EXPOSE_PORTS: 'false' });

    c.autoExposePortsEnabled = true;
    (c as unknown as { autoExposePortsMode: string }).autoExposePortsMode = 'denylist';
    expect(c.buildConfig().env).toEqual({
      SCION_AUTO_EXPOSE_PORTS: 'true',
      SCION_AUTO_EXPOSE_MODE: 'denylist',
      SCION_AUTO_EXPOSE_INTERVAL: '3s',
    });
  });
});

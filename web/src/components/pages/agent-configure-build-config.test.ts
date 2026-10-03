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

// Shared golden fixture (ptone/scion#2493 R5-1): the body buildConfig emits
// when the user edits (adds) one custom env row on an agent whose
// AppliedConfig.Env has an unrelated template key and whose
// InlineConfig.Env-only auto-expose stamp must be re-sent verbatim
// alongside it. Also loaded by the matching Go test
// (TestApplyAgentUpdate_EnvDiffIgnoresUnchangedInlineOnlyStamp), so the two
// cannot drift apart.
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
    // untouched), so the whole `env` key is omitted -- not just the
    // synthesized auto-expose keys -- exactly matching an untouched save
    // leaving CreateInputs' env alone (see TestApplyAgentUpdate_
    // EchoPatchLeavesCreateInputsByteIdentical on the hub side).
    expect(config).not.toHaveProperty('env');
  });

  it('R2-1 facet (a): does not synthesize auto-expose keys when the live env never had them, even after editing an unrelated row', async () => {
    const c = await mountAgentConfigureWithLoadedAgent();
    const withEnvEntries = c as unknown as {
      envEntries: { key: string; value: string }[];
    };
    // Sanity check: the live config (EXPLICIT_KEY only) never had any
    // auto-expose keys, so the control is showing the global default, not a
    // real live value.
    expect(c.autoExposePortsEnabled).toBe(false);

    // Edit the one real explicit key the live config had.
    withEnvEntries.envEntries = [{ key: 'EXPLICIT_KEY', value: 'changed-value' }];

    const config = c.buildConfig();
    expect(config.env).toHaveProperty('EXPLICIT_KEY', 'changed-value');
    // Before the R2-1 fix, this synthesized SCION_AUTO_EXPOSE_PORTS:"false"
    // here even though the agent's live env never set it -- freezing a
    // global default into CreateInputs as an "edit" nobody made.
    expect(config.env).not.toHaveProperty('SCION_AUTO_EXPOSE_PORTS');
    expect(config.env).not.toHaveProperty('SCION_AUTO_EXPOSE_MODE');
  });

  it('R2-1 facet (b): the auto-expose control loads its real live value from ac.env even when ic.env has none, and re-sends that exact value after an unrelated row edit', async () => {
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'claude-opus',
      // ic.env empty while ac.env has everything (the custom key AND the
      // auto-expose key) is the shape an untouched Save/Start used to leave
      // behind before the hub's R4-1 carve-out (applyAgentUpdate) started
      // copying InlineConfig.Env forward -- and could still arise from an
      // agent that went through that window before R4-1 shipped, or from
      // any other future bug that leaves the two maps out of sync. Before
      // R2-1, populateForm read auto-expose from ic.env ONLY, so this shape
      // misread the control as the global default (false) instead of the
      // agent's real, still-live value (true); populateForm must keep
      // getting this right regardless of why the two maps ever diverge.
      env: { EXPLICIT_KEY: 'explicit-value', SCION_AUTO_EXPOSE_PORTS: 'true' },
      inlineConfig: {},
    });
    // The control must reflect the LIVE value, not the global default
    // (stubbed false in stubFetchWithLoadedAgent's settings response).
    expect(c.autoExposePortsEnabled).toBe(true);

    const withEnvEntries = c as unknown as {
      envEntries: { key: string; value: string }[];
    };
    // The custom row must have actually loaded from ac.env (not been lost
    // along with everything else in InlineConfig.Env) before "editing" it
    // means anything.
    expect(withEnvEntries.envEntries).toContainEqual({
      key: 'EXPLICIT_KEY',
      value: 'explicit-value',
    });
    withEnvEntries.envEntries = [{ key: 'EXPLICIT_KEY', value: 'changed-value' }];

    const config = c.buildConfig();
    expect(config.env).toHaveProperty('EXPLICIT_KEY', 'changed-value');
    // Must re-send the REAL loaded value (true), never the global default
    // (false) -- silently flipping live auto-expose off was R2-1 facet (b).
    expect(config.env).toHaveProperty('SCION_AUTO_EXPOSE_PORTS', 'true');
  });

  it('sends telemetry only after the user actually toggles it', async () => {
    const c = await mountAgentConfigureWithLoadedAgent();
    expect(c.telemetryEnabled).toBe(true);
    c.telemetryEnabled = false;
    const config = c.buildConfig();
    expect(config).toHaveProperty('telemetry');
    expect(config.telemetry?.enabled).toBe(false);
  });

  it('sends the auto-expose env keys only after the user actually toggles the control', async () => {
    const c = await mountAgentConfigureWithLoadedAgent();
    expect(c.autoExposePortsEnabled).toBe(false);
    c.autoExposePortsEnabled = true;
    const config = c.buildConfig();
    expect(config.env).toHaveProperty('SCION_AUTO_EXPOSE_PORTS', 'true');
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

describe('agent-configure buildConfig — R4-2: auto-expose control reads the per-key merged env, ic.env taking precedence', () => {
  it('reads the auto-expose stamp from InlineConfig.Env even when AppliedConfig.Env is non-empty for an unrelated (e.g. template) key, and an untouched buildConfig() still equals the golden body', async () => {
    // resolveDerivedConfig (pkg/hub/handlers_agent_create_helpers.go) writes
    // a hub/project auto-expose stamp into InlineConfig.Env only -- it is
    // never aliased into AppliedConfig.Env when the create request had no
    // explicit env of its own. A template's own env can separately make
    // ac.env non-empty (TEMPLATE_KEY here), which must not hide the stamp
    // under the old all-or-nothing `ac.env || ic.env` read.
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { TEMPLATE_KEY: 'x' },
      inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
    });
    // The control must read the InlineConfig.Env stamp (true), not fall
    // back to the global default (false, stubbed in stubFetchWithLoadedAgent)
    // just because ac.env happens to be non-empty for an unrelated reason.
    expect(c.autoExposePortsEnabled).toBe(true);

    const config = c.buildConfig();
    // Nothing was actually edited (TEMPLATE_KEY is an unrelated custom row,
    // and the auto-expose control itself wasn't touched), so buildConfig
    // must still omit `env` entirely and match the untouched golden body
    // exactly -- reading the correct live value must not, by itself, cause
    // it to be echoed.
    expect(config).toEqual(goldenUntouchedBody);
  });

  it('R5-1: re-sends the InlineConfig.Env-only auto-expose stamp verbatim when the user edits an unrelated custom row', async () => {
    // Same live shape as the display test above -- a template key in
    // AppliedConfig.Env, and the auto-expose stamp living only in
    // InlineConfig.Env (exactly as resolveDerivedConfig's project/hub
    // default leaves it) -- but this time the user actually edits a custom
    // row. The hub's recordExplicitEdits (R5-1) depends on the stamp being
    // re-sent at its unchanged value so it is not misread as an edit; this
    // pins the web side of that contract: buildConfig must not drop or
    // alter the stamp just because some other row changed.
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { TEMPLATE_KEY: 'x' },
      inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
    });
    expect(c.autoExposePortsEnabled).toBe(true);

    const withEnvEntries = c as unknown as {
      envEntries: { key: string; value: string }[];
    };
    // The template row is still there (untouched), plus a genuinely new one.
    withEnvEntries.envEntries = [
      { key: 'TEMPLATE_KEY', value: 'x' },
      { key: 'FOO', value: 'bar' },
    ];

    const config = c.buildConfig();
    expect(config).toEqual(goldenRowEditBody);
  });

  it('R6-1: still re-sends the page-visible (InlineConfig) auto-expose value, even when AppliedConfig.Env holds a different value for the same key', async () => {
    // Both maps hold SCION_AUTO_EXPOSE_PORTS, with DIFFERENT values: a
    // template's own env sets it in AppliedConfig.Env (merged there by
    // resolveDerivedConfig), while a project/hub default stamps a different
    // value into InlineConfig.Env only. The control reads the InlineConfig
    // value (R4-2's per-key, ic-wins merge), and that is also what buildConfig
    // must re-send on an unrelated row edit -- the hub's diff (R6-1) depends
    // on seeing exactly the value the page displayed, not AppliedConfig.Env's.
    const c = await mountAgentConfigureWithLoadedAgent({
      model: 'golden-model',
      env: { TEMPLATE_KEY: 'x', SCION_AUTO_EXPOSE_PORTS: 'false' },
      inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
    });
    expect(c.autoExposePortsEnabled).toBe(true);

    const withEnvEntries = c as unknown as {
      envEntries: { key: string; value: string }[];
    };
    withEnvEntries.envEntries = [
      { key: 'TEMPLATE_KEY', value: 'x' },
      { key: 'FOO', value: 'bar' },
    ];

    const config = c.buildConfig();
    // Byte-identical to the R5-1 case: the page shows and re-sends "true"
    // (InlineConfig.Env), never "false" (AppliedConfig.Env).
    expect(config).toEqual(goldenRowEditBody);
  });
});

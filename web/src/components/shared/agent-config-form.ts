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
 * <scion-agent-config-form>: the shared agent configuration form
 * (ptone/scion#3952).
 *
 * One field table, AGENT_CONFIG_FIELDS, drives rendering, validation and
 * emission, so adding a field is one row. The form:
 *
 * - emits only the fields the user touched (collectConfigPatch for the
 *   config object, collectTopLevel for the request's top-level keys).
 *   Touched state is kept per field, never inferred from "value differs from
 *   the loaded one", because Shoelace fires no change event when a user
 *   re-picks the value already shown. For the same reason a field whose
 *   shown value would be an inherited one starts blank, with the inherited
 *   value as a placeholder, so any pick is a change.
 * - treats Clear as "inherit". In edit mode a cleared field is sent as an
 *   explicit null and the hub reverts it to the inherited value; in create
 *   mode it is simply not sent. Limits also offer Unlimited, sent as 0 (or
 *   "0" for a duration); clearing never means unlimited.
 * - shows, per field, whether it is editable now, held, needs a
 *   reincarnation, or is locked (with the reason), from the hub's
 *   editability for the agent and the caller.
 * - shows an unset field's inherited value as a placeholder labelled with
 *   its source, never as the control's value.
 *
 * Create mode renders every field in the five Additional Options tabs. Edit
 * mode renders the fields named by fieldKeys (or every field that has edit
 * semantics) as sections, with a summary strip counting the fields by what
 * the agent's phase does with an edit.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  AgentEditability,
  AgentEditDisposition,
  AgentEditTier,
  AgentFieldEditState,
  AgentInlineConfig,
  AgentResourceSpec,
  CapabilityField,
  HarnessAdvancedCapabilities,
  MessageMode,
  TelemetryConfig,
} from '../../shared/types.js';
import type { AgentConfigPlaceholder } from '../../shared/agent-config-inherited.js';
import type { GcpIdentityState, GcpMetadataMode } from '../../shared/gcp-identity-state.js';
import { MESSAGE_MODE_DISPLAY } from '../../shared/message-mode.js';
import type { EnvEntry } from './env-editor.js';
import './env-editor.js';

export type { AgentConfigPlaceholder } from '../../shared/agent-config-inherited.js';

/** The function groups of the form: the create form's Additional Options tabs. */
export type AgentConfigTab = 'general' | 'auth' | 'prompts' | 'limits' | 'environment';

export const AGENT_CONFIG_TABS: ReadonlyArray<{ id: AgentConfigTab; label: string }> = [
  { id: 'general', label: 'General' },
  { id: 'auth', label: 'Auth & Security' },
  { id: 'prompts', label: 'Prompts' },
  { id: 'limits', label: 'Limits & Resources' },
  { id: 'environment', label: 'Environment & Labels' },
];

/** How a field is edited and emitted. */
export type AgentConfigControl = AgentConfigFieldDef['control'];

/** Keys of AgentInlineConfig whose value is a number. */
type NumberConfigKey = {
  [K in keyof AgentInlineConfig]-?: NonNullable<AgentInlineConfig[K]> extends number ? K : never;
}[keyof AgentInlineConfig];

/** Keys of AgentInlineConfig whose value is a string. */
type StringConfigKey = {
  [K in keyof AgentInlineConfig]-?: NonNullable<AgentInlineConfig[K]> extends string ? K : never;
}[keyof AgentInlineConfig];

/**
 * The config object the form emits: a subset of AgentInlineConfig in which
 * null means "clear, inherit". Typed so the compiler checks that each field
 * emits its key's JSON type (a duration is a string, so Unlimited is "0").
 */
export type AgentConfigPatch = {
  [K in keyof AgentInlineConfig]?: NonNullable<AgentInlineConfig[K]> | null;
};

/** The create request's top-level keys the form owns. */
export interface AgentConfigTopLevel {
  branch?: string;
  agentRole?: string;
  messageMode?: string;
  labels?: Record<string, string>;
  gcp_identity?: { metadata_mode: GcpMetadataMode; service_account_id?: string };
}

/** A choice of a select control. */
export interface AgentConfigOption {
  value: string;
  label: string;
}

interface AgentConfigFieldBase {
  /**
   * The field's key: the hub's editability key ("config.<json key>", or a
   * top-level request key), or a form-only key for a compound control.
   */
  key: string;
  tab: AgentConfigTab;
  label: string;
  /** The field's tier; used when the hub sends no editability (create mode). */
  tier: AgentEditTier;
  help: string;
  /** The harness capability that gates the field, if any. */
  capability?: (caps: HarnessAdvancedCapabilities) => CapabilityField | undefined;
  /**
   * The field has create semantics only: it is not rendered in edit mode.
   * Its edit semantics (what a clear or a removed entry sends) arrive with
   * the Edit page's full field set.
   */
  createOnly?: boolean;
}

/** One row of the field table. control decides how it is edited and emitted. */
export type AgentConfigFieldDef = AgentConfigFieldBase &
  (
    | { control: 'limit-count'; configKey: NumberConfigKey }
    | { control: 'text' | 'textarea' | 'limit-duration'; configKey: StringConfigKey }
    | { control: 'model'; configKey: 'model' }
    | { control: 'thinking'; configKey: 'thinking_level' }
    | { control: 'choice'; configKey: StringConfigKey; options: ReadonlyArray<AgentConfigOption> }
    | {
        control: 'top-choice';
        topKey: 'agentRole' | 'messageMode';
        options: ReadonlyArray<AgentConfigOption>;
      }
    | { control: 'telemetry'; configKey: 'telemetry' }
    | { control: 'resources'; configKey: 'resources' }
    | { control: 'branch' | 'auto-expose' | 'env' | 'labels' | 'gcp-identity' }
  );

/** Model size aliases the harnesses resolve. */
export const MODEL_ALIASES: ReadonlyArray<AgentConfigOption> = [
  { value: 'small', label: 'Small' },
  { value: 'medium', label: 'Medium' },
  { value: 'large', label: 'Large' },
  { value: 'extra-large', label: 'Extra Large' },
];

const TOGGLE_OPTIONS: ReadonlyArray<AgentConfigOption> = [
  { value: 'true', label: 'Enabled' },
  { value: 'false', label: 'Disabled' },
];

const ROLE_OPTIONS: ReadonlyArray<AgentConfigOption> = [
  { value: 'none', label: 'None (no hub access)' },
  { value: 'readonly', label: 'Read-only' },
  { value: 'baseline', label: 'Baseline (standard)' },
  { value: 'full', label: 'Full (requires admin)' },
];

const MESSAGE_MODE_OPTIONS: ReadonlyArray<AgentConfigOption> = (
  Object.entries(MESSAGE_MODE_DISPLAY) as Array<
    [MessageMode, (typeof MESSAGE_MODE_DISPLAY)[MessageMode]]
  >
).map(([mode, d]) => ({ value: mode, label: `${d.label} — ${d.description}` }));

const HARNESS_AUTH_OPTIONS: ReadonlyArray<AgentConfigOption> = [
  { value: 'api-key', label: 'Provider API Key' },
  { value: 'oauth-token', label: 'OAuth Token (env var)' },
  { value: 'vertex-ai', label: 'Vertex Model Garden' },
  { value: 'auth-file', label: 'Harness credential file' },
  { value: 'none', label: 'No Authentication' },
];

/** The five sub-fields of config.resources, by placeholder key. */
const RESOURCE_FIELDS: ReadonlyArray<{
  sub: string;
  label: string;
  example: string;
  get: (r: AgentResourceSpec | undefined) => string | undefined;
}> = [
  {
    sub: 'requests.cpu',
    label: 'CPU request',
    example: 'e.g. 2 or 500m',
    get: (r) => r?.requests?.cpu,
  },
  {
    sub: 'requests.memory',
    label: 'Memory request',
    example: 'e.g. 4Gi',
    get: (r) => r?.requests?.memory,
  },
  { sub: 'limits.cpu', label: 'CPU limit', example: 'e.g. 4', get: (r) => r?.limits?.cpu },
  {
    sub: 'limits.memory',
    label: 'Memory limit',
    example: 'e.g. 8Gi',
    get: (r) => r?.limits?.memory,
  },
  { sub: 'disk', label: 'Disk', example: 'e.g. 20Gi', get: (r) => r?.disk },
];

export const AGENT_CONFIG_FIELDS: ReadonlyArray<AgentConfigFieldDef> = [
  // General
  {
    key: 'branch',
    control: 'branch',
    tab: 'general',
    label: 'Branch',
    tier: 'TX',
    help: "Git branch for this agent's workspace; defaults to the agent name.",
    createOnly: true,
  },
  {
    key: 'config.model',
    configKey: 'model',
    tab: 'general',
    label: 'Model',
    control: 'model',
    tier: 'T1',
    help: 'Model ID or size alias the harness runs.',
  },
  {
    key: 'config.thinking_level',
    configKey: 'thinking_level',
    tab: 'general',
    label: 'Thinking level',
    control: 'thinking',
    tier: 'T1',
    help: '0 is minimal reasoning, 100 is maximum.',
  },
  {
    key: 'config.image',
    configKey: 'image',
    tab: 'general',
    label: 'Container image',
    control: 'text',
    tier: 'T1',
    help: 'Overrides the container image.',
  },
  {
    key: 'config.user',
    configKey: 'user',
    tab: 'general',
    label: 'Container user',
    control: 'text',
    tier: 'T1',
    help: 'Unix user inside the container.',
  },
  {
    key: 'config.telemetry',
    configKey: 'telemetry',
    tab: 'general',
    label: 'Telemetry',
    control: 'telemetry',
    tier: 'T1',
    help: 'Collect telemetry data for this agent.',
    capability: (c) => c.telemetry?.enabled,
  },
  {
    key: 'autoExpose',
    control: 'auto-expose',
    tab: 'general',
    label: 'Auto-expose ports',
    tier: 'T1',
    help: "Detect and expose TCP listening ports from this agent's container.",
    createOnly: true,
  },
  // Auth & Security
  {
    key: 'agentRole',
    control: 'top-choice',
    topKey: 'agentRole',
    options: ROLE_OPTIONS,
    tab: 'auth',
    label: 'Agent role',
    tier: 'T3',
    help: 'Authorization role for hub API access.',
    createOnly: true,
  },
  {
    key: 'messageMode',
    control: 'top-choice',
    topKey: 'messageMode',
    options: MESSAGE_MODE_OPTIONS,
    tab: 'auth',
    label: 'Message mode',
    tier: 'T0',
    help: 'Who this agent can message and be messaged by.',
    createOnly: true,
  },
  {
    key: 'config.auth_selectedType',
    configKey: 'auth_selectedType',
    control: 'choice',
    options: HARNESS_AUTH_OPTIONS,
    tab: 'auth',
    label: 'Harness authentication',
    tier: 'T1',
    help: 'Overrides how the harness authenticates.',
  },
  {
    key: 'gcp_identity',
    control: 'gcp-identity',
    tab: 'auth',
    label: 'GCP Identity',
    tier: 'T3',
    help: '',
    createOnly: true,
  },
  // Prompts
  {
    key: 'config.system_prompt',
    configKey: 'system_prompt',
    control: 'textarea',
    tab: 'prompts',
    label: 'System prompt',
    tier: 'T2',
    help: 'Inline text or a file:// URI.',
    capability: (c) => c.prompts?.system_prompt,
  },
  {
    key: 'config.agent_instructions',
    configKey: 'agent_instructions',
    control: 'textarea',
    tab: 'prompts',
    label: 'Agent instructions',
    tier: 'T2',
    help: 'Inline text or a file:// URI.',
    capability: (c) => c.prompts?.agent_instructions,
  },
  // Limits & Resources
  {
    key: 'config.max_turns',
    configKey: 'max_turns',
    tab: 'limits',
    label: 'Max turns',
    control: 'limit-count',
    tier: 'T1',
    help: 'Stop the agent after this many turns.',
    capability: (c) => c.limits?.max_turns,
  },
  {
    key: 'config.max_model_calls',
    configKey: 'max_model_calls',
    tab: 'limits',
    label: 'Max model calls',
    control: 'limit-count',
    tier: 'T1',
    help: 'Stop the agent after this many model calls.',
    capability: (c) => c.limits?.max_model_calls,
  },
  {
    key: 'config.max_duration',
    configKey: 'max_duration',
    tab: 'limits',
    label: 'Max duration',
    control: 'limit-duration',
    tier: 'T1',
    help: 'Stop the agent after this long, e.g. 30m or 2h.',
    capability: (c) => c.limits?.max_duration,
  },
  {
    key: 'config.resources',
    configKey: 'resources',
    control: 'resources',
    tab: 'limits',
    label: 'Resources',
    tier: 'T1',
    help: 'Container CPU, memory and disk.',
  },
  // Environment & Labels
  {
    key: 'config.env',
    control: 'env',
    tab: 'environment',
    label: 'Environment variables',
    tier: 'T1',
    help: '',
    createOnly: true,
  },
  {
    key: 'labels',
    control: 'labels',
    tab: 'environment',
    label: 'Labels',
    tier: 'T0',
    help: 'Optional key-value labels to organize agents (max 16).',
    createOnly: true,
  },
];

/** Per-field edit state of a single-value control. touched is derived from it, see isTouched. */
interface FieldDraft {
  /** The user typed into or picked in the control. */
  typed: boolean;
  text: string;
  /** The user pressed Clear: inherit. */
  cleared: boolean;
  /** The user chose Unlimited: send 0. */
  unlimited: boolean;
}

function isTouched(d: FieldDraft | undefined): d is FieldDraft {
  return !!d && (d.typed || d.cleared || d.unlimited);
}

/** The auto-expose control's edit state; present once the user operated it. */
interface AutoExposeDraft {
  enabled: '' | 'true' | 'false';
  mode: string;
  list: string;
  interval: string;
}

/** A Go duration, e.g. "90s", "1h30m". */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

/** The disposition of a field the hub gave no editability for (create mode). */
const CREATE_MODE_STATE: AgentFieldEditState = { tier: 'T1', disposition: 'now' };

/** Event detail of `agent-config-change`, fired on every edit. */
export interface AgentConfigChangeDetail {
  /** Keys of the touched fields. */
  touched: string[];
}

/** Other inline config a new or existing agent carries, shown read-only. */
export interface AgentOtherInlineConfig {
  config: Pick<AgentInlineConfig, 'volumes' | 'skills' | 'mcp_servers' | 'services' | 'kubernetes'>;
  /** Where it comes from, e.g. "from the template". */
  source: string;
}

/** One line per kind of other inline config that is set; empty when none is. */
export function otherInlineConfigLines(c: AgentOtherInlineConfig['config'] | undefined): string[] {
  if (!c) return [];
  const lines: string[] = [];
  const n = (count: number, one: string, many: string) => `${count} ${count === 1 ? one : many}`;
  if (c.volumes?.length) lines.push(`Volumes: ${n(c.volumes.length, 'mount', 'mounts')}`);
  if (c.skills?.length) lines.push(`Skills: ${c.skills.map((s) => s.as || s.uri).join(', ')}`);
  const mcp = Object.keys(c.mcp_servers ?? {});
  if (mcp.length) lines.push(`MCP servers: ${mcp.join(', ')}`);
  if (c.services?.length) lines.push(`Services: ${c.services.map((s) => s.name).join(', ')}`);
  if (c.kubernetes && Object.keys(c.kubernetes).length > 0) lines.push('Kubernetes settings');
  return lines;
}

@customElement('scion-agent-config-form')
export class ScionAgentConfigForm extends LitElement {
  /** 'create' renders every field as editable now; 'edit' follows editability. */
  @property() mode: 'create' | 'edit' = 'edit';

  /**
   * The keys of the fields to render. Unset renders every field that has
   * semantics in the current mode. A filter over AGENT_CONFIG_FIELDS, so
   * rendering, validation and emission stay table-driven.
   */
  @property({ attribute: false }) fieldKeys: readonly string[] | null = null;

  /** The stored values (the agent's inline config). */
  @property({ attribute: false }) values: AgentInlineConfig = {};

  /** Inherited values for unset fields, by field key. */
  @property({ attribute: false }) placeholders: Record<string, AgentConfigPlaceholder> = {};

  /** The hub's per-field editability for the agent and the caller. */
  @property({ attribute: false }) editability: AgentEditability | null = null;

  @property({ attribute: false }) harnessCapabilities: HarnessAdvancedCapabilities | null = null;

  /** Disables every control, e.g. while a save is in flight. */
  @property({ type: Boolean }) disabled = false;

  /** Whether the Branch field applies (the project has a git remote, not a shared workspace). */
  @property({ type: Boolean }) branchAvailable = false;

  /** The GCP identity picker's state, owned by the page. */
  @property({ attribute: false }) gcpIdentity: GcpIdentityState | null = null;

  /** Other inline config that is set (volumes, skills, MCP, services, kubernetes). */
  @property({ attribute: false }) otherInlineConfig: AgentOtherInlineConfig | null = null;

  @state() private drafts: Record<string, FieldDraft> = {};
  @state() private resourceDrafts: Record<string, string> = {};
  @state() private autoExpose: AutoExposeDraft | null = null;
  @state() private envEntries: EnvEntry[] | null = null;
  @state() private labelEntries: Array<{ key: string; value: string }> | null = null;
  /** Bumped when the GCP identity state changes, to re-render. */
  @state() private gcpVersion = 0;

  private unsubscribeGcp: (() => void) | null = null;

  /** The rendered fields. */
  get fields(): ReadonlyArray<AgentConfigFieldDef> {
    return AGENT_CONFIG_FIELDS.filter((f) => {
      if (this.fieldKeys) return this.fieldKeys.includes(f.key);
      if (this.mode === 'edit' && f.createOnly) return false;
      if (f.control === 'branch' && !this.branchAvailable) return false;
      if (f.control === 'gcp-identity' && !this.gcpIdentity) return false;
      return true;
    });
  }

  /** Keys of the fields the user touched. */
  get touchedKeys(): string[] {
    return this.fields.filter((f) => this.fieldTouched(f)).map((f) => f.key);
  }

  private fieldTouched(f: AgentConfigFieldDef): boolean {
    switch (f.control) {
      case 'resources':
        return Object.keys(this.resourceDrafts).length > 0;
      case 'auto-expose':
        return this.autoExpose !== null;
      case 'env':
        return this.envEntries !== null;
      case 'labels':
        return this.labelEntries !== null;
      case 'gcp-identity':
        return !!this.gcpIdentity?.gcpIdentityUserSet;
      default:
        return isTouched(this.drafts[f.key]);
    }
  }

  /**
   * The config object for a PATCH or create request: only touched fields.
   * A cleared field is an explicit null in edit mode and absent in create
   * mode; Unlimited is 0.
   */
  collectConfigPatch(): AgentConfigPatch {
    const out: AgentConfigPatch = {};
    const create = this.mode === 'create';
    const env: Record<string, string> = {};
    // The auto-expose control's keys, merged after the custom env entries so
    // the control wins over a custom entry of the same name.
    const autoExposeEnv: Record<string, string> = {};
    let envTouched = false;
    for (const f of this.fields) {
      if (!this.fieldTouched(f) || !editableNow(this.fieldState(f))) continue;
      const d = this.drafts[f.key];
      switch (f.control) {
        case 'limit-count': {
          const v = emitCount(d);
          if (v !== null || !create) out[f.configKey] = v;
          break;
        }
        case 'text':
        case 'textarea':
        case 'limit-duration':
        case 'choice':
        case 'model': {
          const v = emitString(f.control, d);
          if (v !== null || !create) out[f.configKey] = v;
          break;
        }
        case 'thinking': {
          const v = d.cleared ? null : Number.parseInt(d.text, 10);
          if (v !== null || !create) out.thinking_level = v;
          break;
        }
        case 'telemetry': {
          const v: TelemetryConfig | null =
            d.cleared || d.text === '' ? null : { enabled: d.text === 'true' };
          if (v !== null || !create) out.telemetry = v;
          break;
        }
        case 'resources': {
          const v = this.emitResources();
          if (v !== null || !create) out.resources = v;
          break;
        }
        case 'auto-expose': {
          const a = this.autoExpose!;
          if (a.enabled === '') break;
          envTouched = true;
          autoExposeEnv.SCION_AUTO_EXPOSE_PORTS = a.enabled;
          if (a.enabled === 'true') {
            autoExposeEnv.SCION_AUTO_EXPOSE_MODE = a.mode;
            autoExposeEnv.SCION_AUTO_EXPOSE_PORTS_LIST = a.list;
            autoExposeEnv.SCION_AUTO_EXPOSE_INTERVAL = a.interval.trim() || '3s';
          }
          break;
        }
        case 'env':
          for (const e of this.envEntries ?? []) {
            if (e.key) {
              env[e.key] = e.value;
              envTouched = true;
            }
          }
          break;
        default:
          break;
      }
    }
    if (envTouched) out.env = { ...env, ...autoExposeEnv };
    return out;
  }

  /** The create request's top-level keys: only touched fields. */
  collectTopLevel(): AgentConfigTopLevel {
    const out: AgentConfigTopLevel = {};
    for (const f of this.fields) {
      if (!this.fieldTouched(f) || !editableNow(this.fieldState(f))) continue;
      switch (f.control) {
        case 'branch': {
          const v = emitString('text', this.drafts[f.key]);
          if (v) out.branch = v;
          break;
        }
        case 'top-choice': {
          const v = emitString('choice', this.drafts[f.key]);
          if (v) out[f.topKey] = v;
          break;
        }
        case 'labels': {
          const valid = (this.labelEntries ?? []).filter((l) => l.key.trim());
          if (valid.length > 0) {
            out.labels = Object.fromEntries(valid.map((l) => [l.key.trim(), l.value.trim()]));
          }
          break;
        }
        case 'gcp-identity': {
          const v = this.gcpIdentity?.toRequest();
          if (v) out.gcp_identity = v;
          break;
        }
        default:
          break;
      }
    }
    return out;
  }

  /** Validation errors of the touched fields; empty when the form can be sent. */
  validate(): string[] {
    const errors: string[] = [];
    for (const f of this.fields) {
      if (f.control === 'gcp-identity') {
        const err = this.gcpIdentity?.validate();
        if (err) errors.push(err);
        continue;
      }
      const d = this.drafts[f.key];
      if (!isTouched(d) || d.cleared || d.unlimited) continue;
      const text = d.text.trim();
      if (text === '') continue;
      if (f.control === 'limit-count' && !/^[1-9]\d*$/.test(text)) {
        errors.push(`${f.label} must be a whole number greater than 0, or Unlimited.`);
      }
      if (f.control === 'limit-duration' && !DURATION_RE.test(text)) {
        errors.push(`${f.label} must be a duration such as 30m or 2h, or Unlimited.`);
      }
    }
    return errors;
  }

  /** Forgets every edit, e.g. after a successful save. */
  reset(): void {
    this.drafts = {};
    this.resourceDrafts = {};
    this.autoExpose = null;
    this.envEntries = null;
    this.labelEntries = null;
    this.emitChange();
  }

  /** Shows one tab (create mode). */
  showTab(tab: AgentConfigTab): void {
    const group = this.shadowRoot?.querySelector('sl-tab-group') as
      | (Element & { show?: (panel: string) => void })
      | null;
    group?.show?.(tab);
  }

  /** The hub's state for f, narrowed by the harness capability. */
  fieldState(f: AgentConfigFieldDef): AgentFieldEditState {
    let st: AgentFieldEditState =
      this.mode === 'create'
        ? { ...CREATE_MODE_STATE, tier: f.tier }
        : (this.editability?.fields[f.key] ?? {
            tier: f.tier,
            disposition: 'locked',
            reason: 'The hub did not report whether this field can be edited.',
          });
    const cap =
      f.capability && this.harnessCapabilities ? f.capability(this.harnessCapabilities) : undefined;
    if (cap?.support === 'no' && st.disposition !== 'locked') {
      st = {
        ...st,
        disposition: 'locked',
        reason: cap.reason || "This agent's harness does not support this field.",
      };
    }
    return st;
  }

  override willUpdate(changed: Map<string, unknown>): void {
    super.willUpdate(changed);
    if (changed.has('gcpIdentity')) this.subscribeGcp();
  }

  override connectedCallback(): void {
    super.connectedCallback();
    this.subscribeGcp();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.unsubscribeGcp?.();
    this.unsubscribeGcp = null;
  }

  private subscribeGcp(): void {
    this.unsubscribeGcp?.();
    this.unsubscribeGcp = this.gcpIdentity?.subscribe(() => this.gcpVersion++) ?? null;
  }

  private storedText(f: AgentConfigFieldDef): string {
    if (!('configKey' in f)) return '';
    const v = this.values?.[f.configKey];
    if (v === undefined || v === null || v === '' || v === 0) return '';
    if (typeof v === 'object') {
      if (f.control === 'telemetry') {
        const enabled = (v as TelemetryConfig).enabled;
        return enabled === undefined ? '' : String(enabled);
      }
      return '';
    }
    if (this.storedUnlimited(f)) return '';
    return String(v);
  }

  /**
   * Whether the stored value of a limit is Unlimited. Only a duration can
   * show it: "0" is stored, while a count limit of 0 is indistinguishable
   * from unset once saved.
   */
  private storedUnlimited(f: AgentConfigFieldDef): boolean {
    return f.control === 'limit-duration' && this.values?.[f.configKey] === '0';
  }

  private editField(key: string, patch: Partial<FieldDraft>, f: AgentConfigFieldDef): void {
    const prev = this.drafts[key] ?? {
      typed: false,
      text: this.storedText(f),
      cleared: false,
      unlimited: false,
    };
    this.drafts = { ...this.drafts, [key]: { ...prev, ...patch } };
    this.emitChange();
  }

  /**
   * Clear: inherit. In create mode the field simply becomes untouched again
   * (nothing stored to clear); in edit mode it is sent as null.
   */
  private clearField(f: AgentConfigFieldDef): void {
    if (this.mode === 'create') {
      const next = { ...this.drafts };
      delete next[f.key];
      this.drafts = next;
      this.emitChange();
      return;
    }
    this.editField(f.key, { cleared: true, typed: false, unlimited: false, text: '' }, f);
  }

  private emitChange(): void {
    this.dispatchEvent(
      new CustomEvent<AgentConfigChangeDetail>('agent-config-change', {
        detail: { touched: this.touchedKeys },
        bubbles: true,
        composed: true,
      })
    );
  }

  private emitResources(): AgentResourceSpec | null {
    const get = (sub: string): string =>
      (
        this.resourceDrafts[sub] ??
        RESOURCE_FIELDS.find((r) => r.sub === sub)!.get(this.values?.resources) ??
        ''
      ).trim();
    const spec: AgentResourceSpec = {};
    const rc = get('requests.cpu');
    const rm = get('requests.memory');
    const lc = get('limits.cpu');
    const lm = get('limits.memory');
    const disk = get('disk');
    if (rc || rm) spec.requests = { ...(rc ? { cpu: rc } : {}), ...(rm ? { memory: rm } : {}) };
    if (lc || lm) spec.limits = { ...(lc ? { cpu: lc } : {}), ...(lm ? { memory: lm } : {}) };
    if (disk) spec.disk = disk;
    return Object.keys(spec).length > 0 ? spec : null;
  }

  private applyText(): string {
    const phase = this.editability?.phase;
    if (this.mode === 'create' || phase === 'created') return 'Applies at first start';
    if (phase === 'suspended') return 'Applies at resume';
    return 'Applies at next start';
  }

  private dispositionLabel(d: AgentEditDisposition): string {
    switch (d) {
      case 'now':
        return this.editability?.phase === 'suspended' ? 'apply at resume' : 'apply at next start';
      case 'held':
        return 'held until next start';
      case 'reincarnate':
        return 'need reincarnation';
      case 'immediate':
        return 'apply immediately';
      case 'locked':
        return 'locked';
    }
  }

  private renderSummary(): TemplateResult {
    const counts = new Map<AgentEditDisposition, number>();
    for (const f of this.fields) {
      const d = this.fieldState(f).disposition;
      counts.set(d, (counts.get(d) ?? 0) + 1);
    }
    const order: AgentEditDisposition[] = ['now', 'immediate', 'held', 'reincarnate', 'locked'];
    return html`
      <div class="summary" data-testid="tier-summary">
        ${order
          .filter((d) => (counts.get(d) ?? 0) > 0)
          .map(
            (d) =>
              html`<span class="chip chip-${d}" data-disposition=${d}
                >${counts.get(d)} ${this.dispositionLabel(d)}</span
              >`
          )}
      </div>
    `;
  }

  private renderStatus(f: AgentConfigFieldDef, st: AgentFieldEditState, d: FieldDraft | undefined) {
    switch (st.disposition) {
      case 'locked':
        return html`<div class="status locked" data-testid="locked-reason">
          <sl-icon name="lock"></sl-icon><span>${st.reason || 'Not editable.'}</span>
        </div>`;
      case 'held':
        return html`<div class="status">
          <sl-badge variant="warning" pill
            ><sl-icon name="hourglass-split"></sl-icon> Held — applies at next start</sl-badge
          >
        </div>`;
      case 'reincarnate':
        return html`<div class="status">
          <sl-badge variant="neutral" pill
            ><sl-icon name="arrow-repeat"></sl-icon> Needs reincarnation</sl-badge
          >
        </div>`;
      default: {
        const session = st.note === 'session' ? ' (the conversation continues)' : '';
        // A start keeps the previous value for a clear, and for a count
        // limit of 0 (the broker applies limits only when greater than 0).
        // Unlimited on a duration ("0") is applied at the next start.
        const keptByStart = !!d && (d.cleared || (d.unlimited && f.control === 'limit-count'));
        const clearNote =
          st.clearNeedsReincarnate && d && keptByStart
            ? html`<span class="clear-note" data-testid="clear-note">
                ${d.cleared ? 'Clearing' : 'Unlimited'} takes effect at the next reincarnation; a
                plain start keeps the previous value.</span
              >`
            : nothing;
        const help = f.help ? `${f.help} ` : '';
        return html`<div class="status help">
          <span>${help}${this.applyText()}${session}.</span>${clearNote}
        </div>`;
      }
    }
  }

  /** How an inherited value reads in a control: an option's label, else the value. */
  private displayValue(f: AgentConfigFieldDef, value: string): string {
    const options =
      f.control === 'choice' || f.control === 'top-choice'
        ? f.options
        : f.control === 'telemetry' || f.control === 'auto-expose'
          ? TOGGLE_OPTIONS
          : null;
    const match = options?.find((o) => o.value === value);
    return match ? match.label.split(' — ')[0] : value;
  }

  private placeholderFor(f: AgentConfigFieldDef, key = f.key): string {
    const p = this.placeholders?.[key];
    if (!p) return 'Inherited';
    return p.value
      ? `${this.displayValue(f, p.value)} (${p.source})`
      : `Inherited from ${p.source}`;
  }

  private renderClear(f: AgentConfigFieldDef, off: boolean, d: FieldDraft | undefined) {
    const nothingToClear = this.mode === 'create' ? !isTouched(d) : !!d?.cleared;
    return html`<sl-button
      size="small"
      class="clear"
      ?disabled=${off || nothingToClear}
      @click=${() => this.clearField(f)}
      >Clear</sl-button
    >`;
  }

  private renderLabel(f: AgentConfigFieldDef, st: AgentFieldEditState, id: string) {
    return html`<label class="label" for=${id}>
      ${st.disposition === 'locked'
        ? html`<sl-icon name="lock" class="label-lock"></sl-icon>`
        : nothing}
      ${f.label}
      ${this.fieldTouched(f)
        ? html`<span class="edited" data-testid="edited">edited</span>`
        : nothing}
    </label>`;
  }

  private renderField(f: AgentConfigFieldDef): TemplateResult {
    const st = this.fieldState(f);
    const d = this.drafts[f.key];
    // Held and reincarnate-only edits are shown but not yet editable here:
    // saving them needs held edits on the hub, which this form does not
    // send yet.
    const locked =
      st.disposition === 'locked' || st.disposition === 'held' || st.disposition === 'reincarnate';
    const off = this.disabled || locked;
    const id = `input-${f.key.replace(/[^a-zA-Z0-9]+/g, '-')}`;
    let control: TemplateResult;
    switch (f.control) {
      case 'gcp-identity':
        return this.renderGcpIdentity(f);
      case 'env':
        control = this.renderEnv();
        break;
      case 'labels':
        control = this.renderLabels(off);
        break;
      case 'resources':
        control = this.renderResources(off);
        break;
      case 'auto-expose':
        control = this.renderAutoExpose(f, off, id);
        break;
      case 'thinking':
        control = this.renderThinking(f, off, d, id);
        break;
      case 'choice':
      case 'top-choice':
      case 'telemetry':
        control = this.renderChoice(f, off, d, id);
        break;
      case 'model':
        control = this.renderModel(f, off, d, id);
        break;
      default:
        control = this.renderText(f, off, d, id);
    }
    return html`
      <div class="field" data-key=${f.key} data-disposition=${st.disposition} data-tier=${st.tier}>
        ${this.renderLabel(f, st, id)} ${control} ${this.renderOverrideNote(f)}
        ${this.renderStatus(f, st, d)}
      </div>
    `;
  }

  /** A choice that an override setting will replace today is still recorded. */
  private renderOverrideNote(f: AgentConfigFieldDef) {
    const p = this.placeholders?.[f.key];
    if (!p?.override || !this.fieldTouched(f)) return nothing;
    return html`<div class="override-note" data-testid="override-note">
      The ${p.source.split(';')[0]} applies instead of this choice today; the choice is kept and
      applies if that setting is removed.
    </div>`;
  }

  private renderText(f: AgentConfigFieldDef, off: boolean, d: FieldDraft | undefined, id: string) {
    const text = d ? d.text : this.storedText(f);
    const unlimited = d ? d.unlimited : this.storedUnlimited(f);
    const isLimit = f.control === 'limit-count' || f.control === 'limit-duration';
    const placeholder = d?.cleared ? `Cleared — ${this.placeholderFor(f)}` : this.placeholderFor(f);
    const onInput = (e: Event) =>
      this.editField(
        f.key,
        { typed: true, cleared: false, text: (e.target as HTMLInputElement).value },
        f
      );
    return html`<div class="row">
      ${f.control === 'textarea'
        ? html`<sl-textarea
            id=${id}
            size="small"
            rows="5"
            resize="auto"
            .value=${text}
            placeholder=${placeholder}
            ?disabled=${off}
            @sl-input=${onInput}
          ></sl-textarea>`
        : html`<sl-input
            id=${id}
            size="small"
            .value=${unlimited ? '' : text}
            placeholder=${unlimited ? 'Unlimited' : placeholder}
            inputmode=${f.control === 'limit-count' ? 'numeric' : 'text'}
            ?disabled=${off || unlimited}
            @sl-input=${onInput}
          ></sl-input>`}
      ${isLimit
        ? html`<sl-checkbox
            size="small"
            class="unlimited"
            ?checked=${unlimited}
            ?disabled=${off}
            @sl-change=${(e: Event) => {
              const checked = (e.target as HTMLInputElement).checked;
              // Unchecking a stored Unlimited, with nothing typed, removes
              // it: the field reverts to its inherited value.
              const removesStored = !checked && this.storedUnlimited(f) && !d?.typed;
              this.editField(f.key, { unlimited: checked, cleared: removesStored }, f);
            }}
            >Unlimited</sl-checkbox
          >`
        : nothing}
      ${this.renderClear(f, off, d)}
    </div>`;
  }

  /**
   * Model: a free-text model ID plus a size-alias picker. The alias picker
   * starts blank and only fills the text, so any pick is a change.
   */
  private renderModel(f: AgentConfigFieldDef, off: boolean, d: FieldDraft | undefined, id: string) {
    const text = d ? d.text : this.storedText(f);
    const placeholder = d?.cleared ? `Cleared — ${this.placeholderFor(f)}` : this.placeholderFor(f);
    const alias = MODEL_ALIASES.some((a) => a.value === text.trim()) ? text.trim() : '';
    return html`<div class="row">
      <sl-input
        id=${id}
        size="small"
        .value=${text}
        placeholder=${placeholder}
        ?disabled=${off}
        @sl-input=${(e: Event) =>
          this.editField(
            f.key,
            { typed: true, cleared: false, text: (e.target as HTMLInputElement).value },
            f
          )}
      ></sl-input>
      <sl-select
        size="small"
        class="alias"
        placeholder="Size alias"
        .value=${alias}
        ?disabled=${off}
        @sl-change=${(e: Event) => {
          const v = (e.target as HTMLElement & { value: string }).value;
          if (v) this.editField(f.key, { typed: true, cleared: false, text: v }, f);
        }}
      >
        ${MODEL_ALIASES.map((a) => html`<sl-option value=${a.value}>${a.label}</sl-option>`)}
      </sl-select>
      ${this.renderClear(f, off, d)}
    </div>`;
  }

  /** A select that starts blank (inherit), with explicit picks only. */
  private renderChoice(
    f: AgentConfigFieldDef,
    off: boolean,
    d: FieldDraft | undefined,
    id: string
  ) {
    const options =
      f.control === 'choice' || f.control === 'top-choice' ? f.options : TOGGLE_OPTIONS;
    const value = d ? (d.cleared ? '' : d.text) : this.storedText(f);
    const placeholder = d?.cleared ? `Cleared — ${this.placeholderFor(f)}` : this.placeholderFor(f);
    return html`<div class="row">
      <sl-select
        id=${id}
        size="small"
        .value=${value}
        placeholder=${placeholder}
        ?disabled=${off}
        @sl-change=${(e: Event) => {
          const v = (e.target as HTMLElement & { value: string }).value;
          if (v) this.editField(f.key, { typed: true, cleared: false, text: v }, f);
        }}
      >
        ${options.map((o) => html`<sl-option value=${o.value}>${o.label}</sl-option>`)}
      </sl-select>
      ${this.renderClear(f, off, d)}
    </div>`;
  }

  /** Thinking level: inherited until Set is checked; Clear unchecks it. */
  private renderThinking(
    f: AgentConfigFieldDef,
    off: boolean,
    d: FieldDraft | undefined,
    id: string
  ) {
    const stored = this.storedText(f);
    const set = d ? !d.cleared && d.typed : stored !== '';
    const inheritedValue = this.placeholders?.[f.key]?.value;
    // While inherited, the slider shows the inherited value when it is
    // known, and is hidden otherwise: it never shows a made-up position.
    const shown = set ? (d?.typed ? d.text : stored) : inheritedValue;
    const value = shown === undefined ? undefined : Number.parseInt(shown, 10);
    const inherited = d?.cleared ? `Cleared — ${this.placeholderFor(f)}` : this.placeholderFor(f);
    return html`<div class="row">
        ${value === undefined
          ? nothing
          : html`<sl-range
              id=${id}
              min="0"
              max="100"
              step="1"
              .value=${value}
              ?disabled=${off || !set}
              @sl-input=${(e: Event) =>
                this.editField(
                  f.key,
                  {
                    typed: true,
                    cleared: false,
                    text: String((e.target as HTMLElement & { value: number }).value),
                  },
                  f
                )}
            ></sl-range>`}
        <span class="range-value">${set ? value : ''}</span>
        <sl-checkbox
          size="small"
          class="set"
          ?checked=${set}
          ?disabled=${off}
          @sl-change=${(e: Event) => {
            if ((e.target as HTMLInputElement).checked) {
              // Start from the inherited value when known, else the middle.
              const start = inheritedValue ?? '50';
              this.editField(f.key, { typed: true, cleared: false, text: start }, f);
            } else {
              this.clearField(f);
            }
          }}
          >Set</sl-checkbox
        >
      </div>
      ${set ? nothing : html`<div class="inherited" data-testid="inherited">${inherited}</div>`}`;
  }

  private renderResources(off: boolean) {
    return html`<div class="grid">
      ${RESOURCE_FIELDS.map((r) => {
        const key = `config.resources.${r.sub}`;
        const text = this.resourceDrafts[r.sub] ?? r.get(this.values?.resources) ?? '';
        const p = this.placeholders?.[key];
        const placeholder = p?.value ? `${p.value} (${p.source})` : `Inherited (${r.example})`;
        return html`<div class="sub" data-sub=${r.sub}>
          <span class="sub-label">${r.label}</span>
          <sl-input
            size="small"
            .value=${text}
            placeholder=${placeholder}
            ?disabled=${off}
            @sl-input=${(e: Event) => {
              this.resourceDrafts = {
                ...this.resourceDrafts,
                [r.sub]: (e.target as HTMLInputElement).value,
              };
              this.emitChange();
            }}
          ></sl-input>
        </div>`;
      })}
      ${Object.keys(this.resourceDrafts).length > 0
        ? html`<sl-button
            size="small"
            class="clear"
            ?disabled=${off}
            @click=${() => {
              this.resourceDrafts = {};
              this.emitChange();
            }}
            >Clear</sl-button
          >`
        : nothing}
    </div>`;
  }

  private renderAutoExpose(f: AgentConfigFieldDef, off: boolean, id: string) {
    const a = this.autoExpose;
    const update = (patch: Partial<AutoExposeDraft>) => {
      this.autoExpose = {
        ...(a ?? { enabled: '', mode: 'allowlist', list: '', interval: '3s' }),
        ...patch,
      };
      this.emitChange();
    };
    return html`<div class="row">
        <sl-select
          id=${id}
          size="small"
          .value=${a?.enabled ?? ''}
          placeholder=${this.placeholderFor(f)}
          ?disabled=${off}
          @sl-change=${(e: Event) => {
            const v = (e.target as HTMLElement & { value: string })
              .value as AutoExposeDraft['enabled'];
            if (v) update({ enabled: v });
          }}
        >
          ${TOGGLE_OPTIONS.map((o) => html`<sl-option value=${o.value}>${o.label}</sl-option>`)}
        </sl-select>
        <sl-button
          size="small"
          class="clear"
          ?disabled=${off || !a}
          @click=${() => {
            this.autoExpose = null;
            this.emitChange();
          }}
          >Clear</sl-button
        >
      </div>
      ${a?.enabled === 'true'
        ? html`<div class="grid auto-expose-sub">
            <div class="sub">
              <span class="sub-label">Port filter mode</span>
              <sl-select
                size="small"
                class="ae-mode"
                .value=${a.mode}
                ?disabled=${off}
                @sl-change=${(e: Event) =>
                  update({ mode: (e.target as HTMLElement & { value: string }).value })}
              >
                <sl-option value="allowlist">Allowlist</sl-option>
                <sl-option value="denylist">Denylist</sl-option>
              </sl-select>
            </div>
            <div class="sub">
              <span class="sub-label">Port filter list</span>
              <sl-input
                size="small"
                class="ae-list"
                placeholder="e.g. 3000,5173,8080"
                .value=${a.list}
                ?disabled=${off}
                @sl-input=${(e: Event) => update({ list: (e.target as HTMLInputElement).value })}
              ></sl-input>
            </div>
            <div class="sub">
              <span class="sub-label">Scan interval</span>
              <sl-input
                size="small"
                class="ae-interval"
                placeholder="3s"
                .value=${a.interval}
                ?disabled=${off}
                @sl-input=${(e: Event) =>
                  update({ interval: (e.target as HTMLInputElement).value })}
              ></sl-input>
            </div>
          </div>`
        : nothing}`;
  }

  /** The env editor has no disabled state; a locked field is never emitted. */
  private renderEnv() {
    return html`<scion-env-editor
      .entries=${this.envEntries ?? []}
      @env-change=${(e: CustomEvent<{ entries: EnvEntry[] }>) => {
        this.envEntries = e.detail.entries;
        this.emitChange();
      }}
    ></scion-env-editor>`;
  }

  private renderLabels(off: boolean) {
    const entries = this.labelEntries ?? [];
    const set = (next: Array<{ key: string; value: string }>) => {
      this.labelEntries = next;
      this.emitChange();
    };
    return html`<div class="labels">
      ${entries.map(
        (entry, i) => html`
          <div class="label-row">
            <sl-input
              size="small"
              placeholder="key"
              class="label-key"
              .value=${entry.key}
              ?disabled=${off}
              @sl-input=${(e: Event) => {
                const next = [...entries];
                next[i] = { ...next[i], key: (e.target as HTMLInputElement).value };
                set(next);
              }}
            ></sl-input>
            <sl-input
              size="small"
              placeholder="value"
              class="label-value"
              .value=${entry.value}
              ?disabled=${off}
              @sl-input=${(e: Event) => {
                const next = [...entries];
                next[i] = { ...next[i], value: (e.target as HTMLInputElement).value };
                set(next);
              }}
            ></sl-input>
            <sl-icon-button
              name="x-lg"
              label="Remove"
              ?disabled=${off}
              @click=${() => set(entries.filter((_, idx) => idx !== i))}
            ></sl-icon-button>
          </div>
        `
      )}
      ${entries.length < 16
        ? html`<sl-button
            size="small"
            variant="text"
            class="add-label"
            ?disabled=${off}
            @click=${() => set([...entries, { key: '', value: '' }])}
          >
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add label
          </sl-button>`
        : nothing}
    </div>`;
  }

  /**
   * GCP identity: blank with the "No mode chosen" hint until the user picks
   * (unless an applied project default is the outcome); any pick, including
   * Block, is a choice. The rules live in GcpIdentityState.
   */
  private renderGcpIdentity(f: AgentConfigFieldDef): TemplateResult {
    const g = this.gcpIdentity!;
    void this.gcpVersion;
    const off = this.disabled;
    return html`
      <div class="field form-field" data-key=${f.key} data-disposition="now" data-tier=${f.tier}>
        <label class="label">GCP Identity</label>
        <sl-select
          placeholder="Choose an identity..."
          .value=${g.pickerBlank ? '' : g.gcpMetadataMode}
          ?disabled=${off}
          @sl-change=${(e: Event) => {
            const v = (e.target as HTMLElement & { value: string }).value as GcpMetadataMode;
            if (v) g.pickMode(v);
            this.emitChange();
          }}
        >
          ${g.targetKubernetesOnly ? '' : html`<sl-option value="block">Block</sl-option>`}
          ${g.gcpServiceAccounts.length > 0
            ? html`<sl-option value="assign">Assign Service Account</sl-option>`
            : ''}
          <sl-option value="passthrough">Passthrough</sl-option>
        </sl-select>
        <div class="hint">${g.hint}</div>
      </div>
      ${g.gcpMetadataMode === 'assign' && !g.noIdentityModeChosen
        ? html`
            <div class="field form-field" data-key="gcp_identity.service_account">
              <label class="label">Service Account</label>
              ${g.verifiedGCPServiceAccounts.length > 0
                ? html`
                    <sl-select
                      placeholder="Select a service account..."
                      .value=${g.gcpServiceAccountId}
                      ?disabled=${off}
                      @sl-change=${(e: Event) => {
                        g.pickServiceAccount((e.target as HTMLElement & { value: string }).value);
                        this.emitChange();
                      }}
                    >
                      ${g.verifiedGCPServiceAccounts.map(
                        (sa) =>
                          html`<sl-option value=${sa.id}>
                            ${sa.email}${sa.displayName ? ` (${sa.displayName})` : ''}${sa.scope ===
                            'hub'
                              ? ' (Hub)'
                              : ''}
                          </sl-option>`
                      )}
                    </sl-select>
                  `
                : html`
                    <div class="hint">
                      No verified service accounts available. Register and verify service accounts
                      in project settings.
                    </div>
                  `}
            </div>
          `
        : nothing}
    `;
  }

  private renderOtherInlineConfig() {
    const lines = otherInlineConfigLines(this.otherInlineConfig?.config);
    if (lines.length === 0) return nothing;
    return html`<section class="other" data-testid="other-inline-config">
      <h3>
        <sl-icon name="info-circle"></sl-icon>
        Other inline config (${this.otherInlineConfig!.source})
      </h3>
      <p class="other-help">Also applied to this agent; not editable in this form.</p>
      <ul>
        ${lines.map((l) => html`<li>${l}</li>`)}
      </ul>
    </section>`;
  }

  override render() {
    const groups = AGENT_CONFIG_TABS.map((t) => ({
      ...t,
      fields: this.fields.filter((f) => f.tab === t.id),
    }));
    if (this.mode === 'create') {
      return html`
        <sl-tab-group>
          ${groups.map(
            (g, i) => html`<sl-tab slot="nav" panel=${g.id} ?active=${i === 0}>${g.label}</sl-tab>`
          )}
          ${groups.map(
            (g) => html`
              <sl-tab-panel name=${g.id}>
                <section class="group" data-tab=${g.id}>
                  ${g.fields.map((f) => this.renderField(f))}
                </section>
              </sl-tab-panel>
            `
          )}
        </sl-tab-group>
        ${this.renderOtherInlineConfig()}
      `;
    }
    return html`
      ${this.renderSummary()}
      ${groups
        .filter((g) => g.fields.length > 0)
        .map(
          (g) => html`
            <section class="group" data-tab=${g.id}>
              <h3>${g.label}</h3>
              ${g.fields.map((f) => this.renderField(f))}
            </section>
          `
        )}
      ${this.renderOtherInlineConfig()}
    `;
  }

  static override styles = css`
    :host {
      display: block;
    }
    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }
    sl-tab-group::part(body) {
      padding-top: 1.25rem;
    }
    .summary {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
      margin-bottom: 1rem;
    }
    .chip {
      font-size: var(--sl-font-size-small);
      padding: 0.125rem 0.625rem;
      border-radius: var(--sl-border-radius-pill);
      border: 1px solid var(--sl-color-neutral-300);
      background: var(--sl-color-neutral-50);
    }
    .chip-now,
    .chip-immediate {
      border-color: var(--sl-color-success-300);
      background: var(--sl-color-success-50);
    }
    .chip-held,
    .chip-reincarnate {
      border-color: var(--sl-color-warning-300);
      background: var(--sl-color-warning-50);
    }
    .group {
      margin-bottom: 1.25rem;
    }
    .group h3,
    .other h3 {
      font-size: var(--sl-font-size-medium);
      margin: 0 0 0.75rem;
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }
    .field {
      margin-bottom: 1rem;
    }
    .label {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-weight: var(--sl-font-weight-semibold);
      margin-bottom: 0.25rem;
    }
    .edited {
      font-size: var(--sl-font-size-x-small);
      font-weight: normal;
      color: var(--sl-color-primary-600);
    }
    .row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }
    .row sl-input,
    .row sl-select:not(.alias),
    .row sl-textarea,
    .row sl-range {
      flex: 1 1 auto;
      max-width: 28rem;
    }
    .row sl-select.alias {
      width: 9rem;
    }
    .range-value {
      min-width: 2rem;
      text-align: right;
      font-variant-numeric: tabular-nums;
    }
    .grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 0.5rem 1rem;
      max-width: 36rem;
    }
    .sub {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
    }
    .sub-label {
      font-size: var(--sl-font-size-small);
      color: var(--sl-color-neutral-700);
    }
    .auto-expose-sub {
      margin-top: 0.5rem;
    }
    .label-row {
      display: flex;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
      align-items: center;
    }
    .label-row sl-input {
      flex: 1;
    }
    .status,
    .hint,
    .inherited,
    .override-note,
    .other-help {
      margin-top: 0.25rem;
      font-size: var(--sl-font-size-small);
      color: var(--sl-color-neutral-600);
    }
    .status {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
    }
    .status.locked {
      flex-direction: row;
      align-items: center;
      gap: 0.375rem;
    }
    .clear-note,
    .override-note {
      color: var(--sl-color-warning-700);
    }
    .other {
      border-top: 1px solid var(--sl-color-neutral-200);
      padding-top: 0.75rem;
    }
    .other ul {
      margin: 0.25rem 0 0;
      padding-left: 1.25rem;
      font-size: var(--sl-font-size-small);
    }
  `;
}

/** Whether an edit of the field is sent now (not held, not locked). */
function editableNow(st: AgentFieldEditState): boolean {
  return st.disposition === 'now' || st.disposition === 'immediate';
}

/** The value of a touched count limit: null clears, 0 is Unlimited. */
function emitCount(d: FieldDraft): number | null {
  if (d.cleared) return null;
  if (d.unlimited) return 0;
  const text = d.text.trim();
  return text === '' ? null : Number.parseInt(text, 10);
}

/**
 * The value of a touched string field: null clears. Unlimited on a duration
 * is "0", which the hub parses as no limit (a JSON number would not decode
 * into the string field).
 */
function emitString(control: AgentConfigControl, d: FieldDraft): string | null {
  if (d.cleared) return null;
  if (d.unlimited && control === 'limit-duration') return '0';
  const text = control === 'textarea' ? d.text : d.text.trim();
  return text.trim() === '' ? null : text;
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-config-form': ScionAgentConfigForm;
  }
}

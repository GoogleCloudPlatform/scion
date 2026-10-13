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
 * ptone/scion#4928: the web BrokerCapabilities type must use the JSON field
 * names the hub sends for a runtime broker's capabilities object. The fixture
 * lists those names; TestBrokerCapabilitiesFieldsFixture in pkg/store checks
 * it against the store.BrokerCapabilities json tags.
 */
import { describe, expect, it } from 'vitest';
import fixture from '../../../../pkg/store/testdata/broker_capabilities_fields.json';
import type { BrokerCapabilities } from '../../shared/types.js';

/**
 * The keys of BrokerCapabilities. tsc (tsconfig.component-tests.json) fails
 * if this object misses a key of the type or has a key the type lacks.
 */
const WEB_KEYS = {
  webPty: true,
  sync: true,
  attach: true,
} satisfies Record<keyof BrokerCapabilities, true>;

/**
 * Server fields the web type does not declare yet. A new server field fails
 * the test until it is declared in BrokerCapabilities or listed here.
 */
const UNDECLARED_ALLOWED: readonly string[] = [
  'agentMove',
  'asyncLaunch',
  'emptyPerAgentWorkspace',
  'reprovision',
  'reprovisionEmptyPerAgent',
  'startsInFlight',
  'templateTiers',
];

const serverFields: readonly string[] = (fixture as { fields: string[] }).fields;
const serverSet = new Set(serverFields);
const webKeys: readonly string[] = Object.keys(WEB_KEYS);
const webSet = new Set(webKeys);

describe('BrokerCapabilities matches the hub capability JSON fields', () => {
  it('fixture lists server fields', () => {
    const message = 'broker_capabilities_fields.json lists no fields';
    expect(serverFields.length, message).toBeGreaterThan(0);
  });

  it('every web key is a server field', () => {
    const unknown = webKeys.filter((key) => !serverSet.has(key));
    const message = `BrokerCapabilities keys the hub does not send: ${unknown.join(', ')}`;
    expect(unknown, message).toEqual([]);
  });

  it('every undeclared server field is allow-listed', () => {
    const allowed = new Set(UNDECLARED_ALLOWED);
    const unlisted = serverFields.filter((field) => !webSet.has(field) && !allowed.has(field));
    const message = `server fields neither declared nor allow-listed: ${unlisted.join(', ')}`;
    expect(unlisted, message).toEqual([]);
  });

  it('every allow-list entry is an undeclared server field', () => {
    const stale = UNDECLARED_ALLOWED.filter((field) => !serverSet.has(field) || webSet.has(field));
    const message = `allow-listed fields not undeclared server fields: ${stale.join(', ')}`;
    expect(stale, message).toEqual([]);
  });
});

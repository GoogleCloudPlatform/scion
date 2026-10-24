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
 * The rows of the agent-owned artifact journey (spec sections 3 and 5),
 * declared once: they seed the evidence and the oracle row map. Pure data,
 * so the harness self-test can check that the map is complete.
 */

import type { RowSpec } from '../artifacts-access/journey-core.js';

/** A row of this journey: the shared RowSpec plus whether it needs the agent. */
export interface AgentRowSpec extends RowSpec {
  /** Needs a running agent (and so a container runtime); else a declared skip. */
  needsAgent?: boolean;
  /** A STOP row: a failure stops the run with a FINDING. */
  stop?: boolean;
}

/** Generic public text for a non-disclosure row's failures; the detail stays in rows[id].ui. */
export const NON_DISCLOSURE_ERROR =
  'the answer for this artifact differs from the answer for a missing artifact (detail withheld)';

export const ROWS: Record<string, AgentRowSpec> = {
  'R1B-P1': {
    role: '-',
    title: 'member test mode, class test and the marker',
    stop: true,
  },
  'R1B-P2': {
    role: '-',
    title: 'no development sign-in, read back after the restart',
    stop: true,
  },
  'R1B-P3': {
    role: 'local issuer',
    title: 'issuer: the local hub issued the five member identities (recorded deviation)',
    stop: true,
  },
  'R1B-P4': {
    role: 'all five',
    title: 'the runner acts for each test identity (redeem and session asserted)',
    stop: true,
  },
  'R1B-P5': { role: 'aowner', title: 'hub.artifacts is on', stop: true },
  'R1B-P6': {
    role: 'powner',
    title: 'a provider broker that can launch serves the project',
    stop: true,
    needsAgent: true,
  },
  'R1B-P7': {
    role: 'aowner',
    title: 'the agent is created by aowner from the web form and is running',
    stop: true,
    needsAgent: true,
  },
  'R1B-P8': {
    role: 'agent',
    title: 'the agent token carries project:artifact:read and :write before publish',
    stop: true,
    needsAgent: true,
  },
  'R1B-01': {
    role: 'aowner',
    title: 'smoke: the agent publishes from inside; the artifact lands owned by the agent',
    stop: true,
    needsAgent: true,
  },
  'R1B-02': {
    role: 'aowner',
    title: 'delegating user: Grant review access, Retention, Delete; no Edit, Review, Share',
    needsAgent: true,
  },
  'R1B-03': {
    role: 'powner',
    title: 'project owner: Grant review access, Retention, Delete; no Edit, Review, Share',
    needsAgent: true,
  },
  'R1B-04': {
    role: 'padmin',
    title: 'project admin: Grant review access, Delete; no Share, Retention',
    needsAgent: true,
  },
  'R1B-05': {
    role: 'member',
    title: 'member reads only; grants list refused with 403',
    needsAgent: true,
  },
  'R1B-06': {
    role: 'outsider',
    title: 'outsider: the 404 page and the same answers as for a missing artifact',
    needsAgent: true,
    genericError: NON_DISCLOSURE_ERROR,
  },
  'R1B-07': {
    role: 'aowner, member',
    title: 'History: v1 published by the agent',
    needsAgent: true,
  },
  'R1B-08a': {
    role: 'aowner -> member',
    title: 'Grant review access dialog gives the member Can review',
    needsAgent: true,
  },
  'R1B-08b': {
    role: 'padmin -> outsider',
    title: 'review grant for a person who cannot read: 403 review_subject_cannot_read (API)',
    needsAgent: true,
  },
  'R1B-09': {
    role: 'member',
    title: 'review grantee sees Review only and saves a review; the owner is unchanged',
    needsAgent: true,
  },
  'R1B-10': {
    role: 'aowner, powner',
    title: 'after the review: owner still the agent, Review pending, controls unchanged',
    needsAgent: true,
  },
  'R1B-11': {
    role: 'aowner',
    title: 'aowner deletes the agent from the agent page',
    needsAgent: true,
  },
  'R1B-12': {
    role: 'member, aowner',
    title: 'observation: right after the delete, before the owner pass',
    graded: false,
    needsAgent: true,
  },
  'R1B-13': {
    role: 'member',
    title: 'the owner-deleted mark arrives (15 s polls, 15 min deadline, no trigger)',
    needsAgent: true,
  },
  'R1B-14': {
    role: 'aowner, powner, padmin, member',
    title: 'Owner deleted badge and struck owner; readers still read; grants unchanged',
    needsAgent: true,
  },
  'R1B-15': {
    role: 'outsider',
    title: 'outsider still gets the 404 page, the same as for a missing artifact',
    needsAgent: true,
    genericError: NON_DISCLOSURE_ERROR,
  },
  'R1B-16': {
    role: 'powner, padmin, member (aowner observed)',
    title: 'controls after the owner is gone (aowner leg is an observation, Q-A5)',
    needsAgent: true,
  },
  'R1B-17': {
    role: 'padmin, powner',
    title: 'cleanup by absence: artifact (UI Delete), agent, memberships, project',
    where: 'cleanup',
  },
  'R1B-18': {
    role: '-',
    title: 'evidence after cleanup; no traces, HAR, video, screenshots, tokens or codes',
    where: 'cleanup',
  },
};

export const ROW_IDS = Object.keys(ROWS);

/** Rows that need a running agent. */
export const AGENT_ROW_IDS = ROW_IDS.filter((id) => ROWS[id].needsAgent);

/** Reason of the declared skip where no container runtime is usable (the b02 gate tier). */
export function noRuntimeReason(launchCapableReason: string): string {
  return `NOT RUN: no container runtime in this lane (${launchCapableReason})`;
}

/** The two tests of the journey. */
export const TITLE_JOURNEY = 'R1-B: agent-owned artifact access per role, graded at the UI';
export const TITLE_CLEANUP = 'R1B-17/R1B-18: cleanup by absence, then the evidence';

/** Every row mapped to the test that carries it. */
export const ORACLE_ROW_MAP: Record<string, { test: string; note: string }> = Object.fromEntries(
  ROW_IDS.map((id) => {
    const spec = ROWS[id];
    if (spec.where === 'cleanup') {
      return [id, { test: TITLE_CLEANUP, note: `the afterAll hook: evidence.rows["${id}"]` }];
    }
    const kind = spec.graded === false ? 'observation, not graded' : 'a test step';
    const stop = spec.stop ? '; STOP row' : '';
    return [id, { test: TITLE_JOURNEY, note: `evidence.rows["${id}"] (${kind}${stop})` }];
  })
);

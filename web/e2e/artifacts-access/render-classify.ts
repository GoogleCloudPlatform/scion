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
 * Which requests seen from the bundle frame the session checks count. They
 * count requests started by the frame's content, and exclude exactly one
 * class: the iframe element's own document load started by the app page.
 * That class is recognised only when ALL of these hold, read from the
 * request's own full headers:
 *   - Sec-Fetch-Site is same-origin;
 *   - Sec-Fetch-Dest is iframe;
 *   - Sec-Fetch-Mode is navigate;
 *   - the URL is exactly a frame src the app set for a mint.
 * Anything missing, unread or different is frame-initiated (fail-safe).
 */

export type FrameRequestClass = 'excluded-parent-load' | 'frame-initiated';

/** What the classifier reads from one request. */
export interface ClassifierInput {
  /** The full request headers were read (false: unknown, so graded). */
  headersRead: boolean;
  secFetchSite: string | null;
  secFetchDest: string | null;
  secFetchMode: string | null;
  /** The request's full URL (in memory only). */
  url: string | null;
}

/**
 * Classifies one request against the frame srcs the app set (absolute
 * URLs). Returns the class and whether the URL matched a src.
 */
export function classifyFrameRequest(
  q: ClassifierInput,
  frameSrcs: readonly string[]
): { classification: FrameRequestClass; srcMatch: boolean } {
  const srcMatch = q.url !== null && frameSrcs.includes(q.url);
  const excluded =
    q.headersRead &&
    q.secFetchSite === 'same-origin' &&
    q.secFetchDest === 'iframe' &&
    q.secFetchMode === 'navigate' &&
    srcMatch;
  return { classification: excluded ? 'excluded-parent-load' : 'frame-initiated', srcMatch };
}

/** One self-test case: an input and the class it must get. */
interface SelfTestCase {
  name: string;
  input: ClassifierInput;
  want: FrameRequestClass;
}

const SRC = 'https://hub.example/api/v1/artifacts/view/CAP/index.html';
const APP_LOAD: ClassifierInput = {
  headersRead: true,
  secFetchSite: 'same-origin',
  secFetchDest: 'iframe',
  secFetchMode: 'navigate',
  url: SRC,
};

/**
 * The classifier's self-test, run before any identity exists: the app's
 * load is excluded; each condition failing on its own, a missing header and
 * unread headers each make the request frame-initiated. Returns each case's
 * result; ok is false if any case got the wrong class.
 */
export function classifierSelfTest(): {
  ok: boolean;
  cases: Array<{ name: string; want: FrameRequestClass; got: FrameRequestClass }>;
} {
  const cases: SelfTestCase[] = [
    { name: 'app-initiated src load', input: APP_LOAD, want: 'excluded-parent-load' },
    {
      name: 'site cross-site',
      input: { ...APP_LOAD, secFetchSite: 'cross-site' },
      want: 'frame-initiated',
    },
    {
      name: 'dest document',
      input: { ...APP_LOAD, secFetchDest: 'document' },
      want: 'frame-initiated',
    },
    {
      name: 'mode no-cors',
      input: { ...APP_LOAD, secFetchMode: 'no-cors' },
      want: 'frame-initiated',
    },
    {
      name: 'url not a src (in-frame link)',
      input: { ...APP_LOAD, url: 'https://hub.example/api/v1/artifacts/view/CAP/page2.html' },
      want: 'frame-initiated',
    },
    { name: 'site missing', input: { ...APP_LOAD, secFetchSite: null }, want: 'frame-initiated' },
    { name: 'dest missing', input: { ...APP_LOAD, secFetchDest: null }, want: 'frame-initiated' },
    { name: 'mode missing', input: { ...APP_LOAD, secFetchMode: null }, want: 'frame-initiated' },
    { name: 'url missing', input: { ...APP_LOAD, url: null }, want: 'frame-initiated' },
    { name: 'headers unread', input: { ...APP_LOAD, headersRead: false }, want: 'frame-initiated' },
  ];
  const results = cases.map((c) => ({
    name: c.name,
    want: c.want,
    got: classifyFrameRequest(c.input, [SRC]).classification,
  }));
  return { ok: results.every((x) => x.got === x.want), cases: results };
}

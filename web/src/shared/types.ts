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
 * Shared types for server and client
 */

/**
 * User information
 */
export interface User {
  id: string;
  email: string;
  name: string;
  avatar?: string | undefined;
}

/**
 * Initial page data passed from SSR to client
 */
export interface PageData {
  /** Current URL path */
  path: string;
  /** Page title */
  title: string;
  /** Current user (if authenticated) */
  user?: User | undefined;
  /** Additional page-specific data */
  data?: Record<string, unknown> | undefined;
}

/**
 * Route definition for client-side routing
 */
export interface RouteConfig {
  path: string;
  component: string;
  action?: () => Promise<void>;
}

/**
 * Grove status enumeration
 */
export type GroveStatus = 'active' | 'inactive' | 'error';

/**
 * Grove information from the Hub API
 */
export interface Grove {
  id: string;
  name: string;
  path: string;
  status: GroveStatus;
  agentCount: number;
  createdAt: string;
  updatedAt: string;
}

/**
 * Agent status enumeration
 */
export type AgentStatus = 'running' | 'stopped' | 'provisioning' | 'error';

/**
 * Agent session status
 */
export type AgentSessionStatus = 'idle' | 'active' | 'busy' | 'disconnected';

/**
 * Agent information from the Hub API
 */
export interface Agent {
  id: string;
  name: string;
  groveId: string;
  template: string;
  status: AgentStatus;
  sessionStatus?: AgentSessionStatus;
  taskSummary?: string;
  createdAt: string;
  updatedAt: string;
}

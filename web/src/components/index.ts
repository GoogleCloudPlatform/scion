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
 * Component exports
 *
 * Re-exports all Lit components for easy importing
 */

// App shell
export { ScionApp } from './app-shell.js';

// Shared components
export { ScionNav, ScionHeader, ScionBreadcrumb, ScionStatusBadge, ScionViewToggle } from './shared/index.js';
export type { StatusType } from './shared/index.js';
export type { ViewMode } from './shared/index.js';

// Pages
export { ScionPageHome } from './pages/home.js';
export { ScionPageProjects } from './pages/projects.js';
export { ScionPageProjectDetail } from './pages/project-detail.js';
export { ScionPageAgents } from './pages/agents.js';
export { ScionPageAgentDetail } from './pages/agent-detail.js';
export { ScionPageAgentCreate } from './pages/agent-create.js';
export { ScionPageProjectCreate } from './pages/project-create.js';
export { ScionPageProjectSettings } from './pages/project-settings.js';
export { ScionPageProjectSchedules } from './pages/project-schedules.js';
export { ScionPageBrokers } from './pages/brokers.js';
export { ScionPageAdminUsers } from './pages/admin-users.js';
export { ScionPageAdminGroups } from './pages/admin-groups.js';
export { ScionPageProfileEnvVars } from './pages/profile-env-vars.js';
export { ScionPageProfileSecrets } from './pages/profile-secrets.js';
export { ScionPageSettings } from './pages/settings.js';
export { ScionPage404 } from './pages/not-found.js';
export { ScionLoginPage } from './pages/login.js';

// Profile shell
export { ScionProfileShell } from './profile/profile-shell.js';
export { ScionProfileNav } from './profile/profile-nav.js';

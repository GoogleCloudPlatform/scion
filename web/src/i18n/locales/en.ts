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

/** Source catalog. Keys describe meaning, never routes or API identifiers. */
export const en = {
  'nav.overview': 'Overview',
  'nav.management': 'Management',
  'nav.admin': 'Admin',
  'nav.tagline': 'Agent Orchestration',
  'nav.expand': 'Expand sidebar',
  'nav.collapse': 'Collapse sidebar',
  'nav.collapseShort': 'Collapse',
  'page.dashboard': 'Dashboard',
  'page.projects': 'Projects',
  'page.agents': 'Agents',
  'page.terminals': 'Terminals',
  'page.brokers': 'Brokers',
  'page.settings': 'Settings',
  'page.hubResources': 'Hub Resources',
  'page.scheduler': 'Scheduler',
  'page.users': 'Users',
  'page.groups': 'Groups',
  'page.serverConfig': 'Server Config',
  'page.federation': 'Federation',
  'page.integrations': 'Integrations',
  'page.roles': 'Roles',
  'page.roleBindings': 'Role Bindings',
  'page.accessConstraints': 'Access Constraints',
  'page.quotas': 'Quotas',
  'page.health': 'Health',
  'page.metrics': 'Metrics',
  'page.skillRegistries': 'Skill Registries',
  'page.skills': 'Skills',
  'page.githubSetup': 'GitHub App Setup',
  'page.setup': 'Setup',
  'page.createProject': 'Create Project',
  'page.projectSettings': 'Project Settings',
  'page.schedules': 'Schedules',
  'page.projectMetrics': 'Project Metrics',
  'page.artifact': 'Artifact',
  'page.artifacts': 'Artifacts',
  'page.template': 'Template',
  'page.project': 'Project',
  'page.createAgent': 'Create Agent',
  'page.terminal': 'Terminal',
  'page.editAgent': 'Edit Agent',
  'page.agent': 'Agent',
  'page.broker': 'Broker',
  'page.harnessConfig': 'Harness Config',
  'page.group': 'Group',
  'page.maintenance': 'Maintenance',
  'page.integration': 'Integration',
  'page.createSkill': 'Create Skill',
  'page.skill': 'Skill',
  'page.skillRegistry': 'Skill Registry',
  'page.notFound': 'Page Not Found',
  'page.diagnostics': 'Diagnostics',
  'breadcrumb.home': 'Home',
  'header.navigation': 'Open navigation menu',
  'header.notifications': 'Notifications',
  'header.help': 'Help',
  'header.toggleTheme': 'Toggle dark mode',
  'header.profile': 'Profile',
  'header.signOut': 'Sign out',
  'header.signOutMenu': 'Sign Out',
  'header.signIn': 'Sign in',
  'header.switchView': 'Switch view',
  'header.switchMode': 'Switch view mode',
  'header.switchModeUnread': 'Switch view mode, {{unread}}',
  'header.terminalsCount': 'Terminals ({{count}})',
  'header.chat': 'Chat',
  'header.chatUnread': 'Chat, {{unread}}',
  'header.unread_one': '{{count}} unread conversation',
  'header.unread_other': '{{count}} unread conversations',
  'header.account': 'Account menu',
  'header.lightMode': 'Light Mode',
  'header.darkMode': 'Dark Mode',
  'header.jumpToAgent': 'Jump to agent ({{shortcut}})',
  'header.openJumpToAgent': 'Open Jump to agent',
  'language.label': 'Language',
  'language.auto': 'System language',
} as const;

type PluralCategory = 'zero' | 'one' | 'two' | 'few' | 'many' | 'other';
type BaseKey<K> = K extends `${infer Base}_${PluralCategory}` ? Base : K;
export type MessageKey = BaseKey<keyof typeof en>;
/** Other languages can have more plural forms than English. */
export type MessageCatalog = Partial<
  Record<MessageKey | `${MessageKey}_${PluralCategory}`, string>
>;

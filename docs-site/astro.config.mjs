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

// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import d2 from 'astro-d2';
import starlightLinksValidator from 'starlight-links-validator';

// The Hosted user journey is shared by both hosted tiers (Single-node and HA).
// The pages live once under `hosted/user/`; both tiers reference the same slugs
// so we link rather than duplicate.
const hostedUserGuide = {
	label: 'User Guide',
	items: [
		{ label: 'Connecting to a Hub', slug: 'hosted/user/hosted-user' },
		{ label: 'User Access Tokens', slug: 'hosted/user/personal-access-tokens' },
		{ label: 'Secrets & Environment', slug: 'hosted/user/secrets' },
		{ label: 'Messaging & Notifications', slug: 'hosted/user/messaging' },
		{ label: 'External Channels', slug: 'hosted/user/external-channels' },
	],
};

// https://astro.build/config
export default defineConfig({
	site: 'https://googlecloudplatform.github.io',
	base: '/scion',
	// Preserve old URLs after the mode-axis restructure so inbound links do not 404.
	redirects: {
		'/advanced-local/local-governance': '/local/local-governance',
		'/advanced-local/agent-lifecycle': '/local/agent-lifecycle',
		'/advanced-local/workspace': '/local/workspace',
		'/advanced-local/templates': '/local/templates',
		'/advanced-local/agent-credentials': '/local/agent-credentials',
		'/advanced-local/custom-images': '/local/custom-images',
		'/advanced-local/tmux': '/local/tmux',
		'/advanced-local/completions': '/local/completions',
		'/advanced-local/workstation-server': '/workstation/workstation-server',
		'/workspaces-and-sharing': '/local/workspaces-and-sharing',
		'/hub-user/dashboard': '/workstation/dashboard',
		'/hub-user/git-projects': '/workstation/git-projects',
		'/hub-user/hosted-user': '/hosted/user/hosted-user',
		'/hub-user/personal-access-tokens': '/hosted/user/personal-access-tokens',
		'/hub-user/secrets': '/hosted/user/secrets',
		'/hub-user/messaging': '/hosted/user/messaging',
		'/hub-user/external-channels': '/hosted/user/external-channels',
		'/hub-user/runtime-broker': '/hosted/ha/runtime-broker',
		'/hub-user/multi-broker': '/hosted/ha/multi-broker',
		'/hub-admin/hub-server': '/hosted/single-node/hub-server',
		'/hub-admin/hub-setup-gce': '/hosted/single-node/hub-setup-gce',
		'/hub-admin/auth': '/hosted/single-node/auth',
		'/hub-admin/observability': '/hosted/single-node/observability',
		'/hub-admin/metrics': '/hosted/single-node/metrics',
		'/hub-admin/kubernetes': '/hosted/ha/kubernetes',
		'/hub-admin/auth-proxy-iap': '/hosted/ha/auth-proxy-iap',
		'/hub-admin/permissions': '/hosted/ha/permissions',
		'/hub-admin/lifecycle-hooks': '/hosted/ha/lifecycle-hooks',
		'/development/logging': '/contributing/logging',
	},
	integrations: [
		d2(),
		starlight({
			plugins: [starlightLinksValidator()],
			title: 'Scion',
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/GoogleCloudPlatform/scion' },
			],
			sidebar: [
				{
					label: 'Introduction & Foundations',
					items: [
						{ label: 'Overview', slug: 'overview' },
						{ label: 'Choosing a Mode', slug: 'choosing-a-mode' },
						{ label: 'Core Concepts', slug: 'concepts' },
						{ label: 'Philosophy', slug: 'philosophy' },
						{ label: 'Supported Harnesses', slug: 'supported-harnesses' },
						{ label: 'Glossary', slug: 'glossary' },
						{ label: 'Release Notes', slug: 'release-notes' },
					],
				},
				{
					label: 'Getting Started (Workstation)',
					items: [
						{ label: 'Installation', slug: 'getting-started/install' },
						{ label: 'Onboarding Wizard', slug: 'getting-started/onboarding' },
						{ label: 'Tutorial', slug: 'getting-started/tutorial' },
					],
				},
				{
					label: 'Local Mode',
					items: [
						{ label: 'Local Configuration', slug: 'local/local-governance' },
						{ label: 'Working with Agents', slug: 'local/agent-lifecycle' },
						{ label: 'About Workspaces', slug: 'local/workspace' },
						{ label: 'Workspaces & Sharing Modes', slug: 'local/workspaces-and-sharing' },
						{ label: 'Templates & Roles', slug: 'local/templates' },
						{ label: 'Skills (Authoring & Publishing)', slug: 'local/skills' },
						{ label: 'Agent Credentials', slug: 'local/agent-credentials' },
						{ label: 'Custom Images', slug: 'local/custom-images' },
						{ label: 'Tmux Sessions', slug: 'local/tmux' },
						{ label: 'Apple Container DNS', slug: 'local/apple-container' },
						{ label: 'Shell Completions', slug: 'local/completions' },
					],
				},
				{
					label: 'Workstation Mode',
					items: [
						{ label: 'The Combo Server', slug: 'workstation/workstation-server' },
						{ label: 'The Web Dashboard', slug: 'workstation/dashboard' },
						{ label: 'Projects (Hub-managed vs Linked)', slug: 'workstation/git-projects' },
					],
				},
				{
					label: 'Hosted — Single-node',
					items: [
						hostedUserGuide,
						{
							label: 'Admin Guide',
							items: [
								{ label: 'Single-node Overview', slug: 'hosted/single-node/overview' },
								{ label: 'Hub Setup', slug: 'hosted/single-node/hub-server' },
								{ label: 'Deploy on a VM (GCE)', slug: 'hosted/single-node/hub-setup-gce' },
								{ label: 'Auth & Tenancy', slug: 'hosted/single-node/auth' },
								{ label: 'Managed Agents', slug: 'hosted/single-node/managed-agents' },
								{ label: 'Skill Registry & Federation', slug: 'hosted/single-node/skill-registry' },
								{ label: 'Observability', slug: 'hosted/single-node/observability' },
								{ label: 'Metrics', slug: 'hosted/single-node/metrics' },
							],
						},
					],
				},
				{
					label: 'Hosted — HA',
					items: [
						hostedUserGuide,
						{
							label: 'Admin Guide',
							items: [
								{ label: 'HA Overview', slug: 'hosted/ha/overview' },
								{ label: 'Kubernetes Runtime', slug: 'hosted/ha/kubernetes' },
								{ label: 'Runtime Brokers & Profiles', slug: 'hosted/ha/runtime-broker' },
								{ label: 'Managed Agents', slug: 'hosted/single-node/managed-agents' },
								{ label: 'Skill Registry & Federation', slug: 'hosted/single-node/skill-registry' },
								{ label: 'Multi-Broker Setup', slug: 'hosted/ha/multi-broker' },
								{ label: 'Identity & Access (RBAC)', slug: 'hosted/ha/permissions' },
								{ label: 'Proxy Auth (IAP)', slug: 'hosted/ha/auth-proxy-iap' },
								{ label: 'Lifecycle Hooks', slug: 'hosted/ha/lifecycle-hooks' },
								{ label: 'Observability', slug: 'hosted/single-node/observability' },
								{ label: 'Metrics', slug: 'hosted/single-node/metrics' },
							],
						},
					],
				},
				{
					label: 'Technical Reference',
					autogenerate: { directory: 'reference' },
				},
				{
					label: 'Contributing',
					autogenerate: { directory: 'contributing' },
				},
			],
		}),
	],
});

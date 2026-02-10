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

// https://astro.build/config
export default defineConfig({
	integrations: [
		d2(),
		starlight({
			title: 'Scion',
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/google/scion' },
			],
			sidebar: [
				{
					label: 'Foundations',
					items: [
						{ label: 'Overview', slug: 'overview' },
						{ label: 'Core Concepts', slug: 'concepts' },
						{ label: 'Supported Harnesses', slug: 'supported-harnesses' },
						{ label: 'Glossary', slug: 'glossary' },
					],
				},
				{
					label: 'Developer Guide',
					items: [
						{
							label: 'Local Workflow',
							items: [
								{ label: 'Installation', slug: 'install' },
								{ label: 'Workspace Management', slug: 'guides/workspace' },
							],
						},
						{
							label: 'Team Workflow',
							items: [
								{ label: 'Connecting to Hub', slug: 'guides/hosted-user' },
								{ label: 'Web Dashboard', slug: 'guides/dashboard' },
								{ label: 'Secret Management', slug: 'guides/secrets' },
							],
						},
						{
							label: 'Skills',
							items: [
								{ label: 'Templates', slug: 'guides/templates' },
								{ label: 'Tmux Sessions', slug: 'guides/tmux' },
							],
						},
					],
				},
				{
					label: 'Operations & Hosting',
					items: [
						{ label: 'Local Governance', slug: 'guides/local-governance' },
						{ label: 'Hub Setup', slug: 'guides/hub-server' },
						{ label: 'Runtime Broker', slug: 'guides/runtime-broker' },
						{ label: 'Kubernetes', slug: 'guides/kubernetes' },
						{ label: 'Security', slug: 'guides/auth' },
						{ label: 'Permissions', slug: 'guides/permissions' },
						{ label: 'Observability', slug: 'guides/observability' },
						{ label: 'Metrics', slug: 'guides/metrics' },
					],
				},
				{
					label: 'Reference',
					autogenerate: { directory: 'reference' },
				},
				{
					label: 'Development',
					items: [
						{ label: 'Local Logging', slug: 'development/logging' },
					],
				},
				{
					label: 'Contributing',
					autogenerate: { directory: 'contributing' },
				},
			],
		}),
	],
});

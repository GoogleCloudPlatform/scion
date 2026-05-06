---
name: rails-expert
description: Specialized expert for Ruby on Rails development. Use when working with Rails applications, generating models/migrations, writing ActiveRecord queries, refactoring with idioms (Service Objects, Concerns), or writing RSpec/Minitest tests.
---

# Rails Expert

## Overview

This skill enables high-velocity, idiomatic Ruby on Rails development. It provides guidance on "The Rails Way," modern patterns (Service Objects, Concerns), and rigorous testing standards.

## Core Tasks

### 1. Model & Migration Design
When designing models, prioritize ActiveRecord conventions:
- Use expressive migration names.
- Ensure proper indexing for foreign keys and frequently queried columns.
- Define associations (`belongs_to`, `has_many`) and validations early.

### 2. Idiomatic Refactoring
Keep controllers and models "thin" by delegating complexity:
- **Service Objects**: For multi-step business logic. See [idioms.md](references/idioms.md).
- **Concerns**: For shared behavior across models/controllers. See [idioms.md](references/idioms.md).
- **Service Template**: Use the boilerplate at [service_object.rb](assets/service_object.rb).

### 3. ActiveRecord Performance
Avoid common pitfalls:
- **N+1 Queries**: Always use `includes`, `preload`, or `eager_load` for associated records.
- **Query Objects**: Extract complex SQL or ActiveRecord chains into dedicated classes. See [idioms.md](references/idioms.md).

### 4. Expressive Testing
Write tests that document behavior:
- **RSpec**: Use `describe`/`context` blocks and named subjects. See [testing.md](references/testing.md).
- **Minitest**: Follow standard Rails integration and unit test patterns.
- **FactoryBot**: Use traits for maintainable test data setup.

## Reference Materials

- [idioms.md](references/idioms.md): Detailed guide on Service Objects, Concerns, and Query Objects.
- [testing.md](references/testing.md): Best practices for RSpec, FactoryBot, and System Tests.
- [ecosystem.md](references/ecosystem.md): Standard ecosystem libraries (Grape, Faraday, Sidekiq, Airbrake) usage guidelines.

## Templates

- [service_object.rb](assets/service_object.rb): Standard boilerplate for creating new Service Objects.

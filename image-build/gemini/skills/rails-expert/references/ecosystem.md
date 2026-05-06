# Ruby on Rails Ecosystem & Libraries

Based on the standard stack utilized across the repositories, adhere to the following established patterns and libraries when developing or refactoring:

## API Framework (Grape)
- **Usage**: Several API services utilize **Grape** for building REST-like APIs instead of standard ActionControllers.
- **Pattern**: APIs are defined using Grape DSL. Pay attention to parameter validation (`requires`, `optional`), Grape helpers, and entity representations.

## Network Requests (Faraday)
- **HTTP Client**: Use **Faraday** for outbound HTTP network requests.
- **Adapters**: The ecosystem leverages various Faraday middleware and adapters (e.g., `faraday_curl`). Stick to configuring standard Faraday connections for external integrations rather than `Net::HTTP` or `HTTParty`.

## Background Jobs (Sidekiq & Shoryuken)
- **Sidekiq**: Used for standard asynchronous Redis-based job processing. Use `include Sidekiq::Worker` (or ActiveJob depending on context) and prefer passing simple identifiers (like IDs) rather than full ActiveRecord objects.
- **Shoryuken**: Used for AWS SQS-based worker queues. Follow standard AWS/Shoryuken polling patterns when adding SQS workers.

## Observability & Error Tracking
- **APM**: **ScoutAPM** is universally used for application performance monitoring. Ensure performance-critical paths are monitorable.
- **Error Tracking**: **Airbrake** (and occasionally Rollbar) is used for exception tracking. Ensure rescue blocks properly report unexpected errors (e.g., `Airbrake.notify`) rather than swallowing them silently.

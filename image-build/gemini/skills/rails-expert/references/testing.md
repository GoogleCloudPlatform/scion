# Testing Standards (RSpec & Minitest)

## RSpec Best Practices
- **Descriptive Specs**: Use `describe` for classes/methods and `context` for state.
- **Let vs Let!**: Use `let` for lazy loading and `let!` when setup must happen before each example.
- **Subject**: Use named subjects for clarity: `subject(:user) { User.new }`.
- **Matchders**: Use built-in matchers (e.g., `be_valid`, `change { ... }.by(1)`) or custom matchers for domain logic.

## FactoryBot Patterns
- **Keep it Simple**: Factories should provide valid minimum state.
- **Traits**: Use traits for specific states (e.g., `trait :admin { admin { true } }`).
- **Sequences**: Use sequences for unique fields like emails.

## System Tests
- Use Capybara for end-to-end testing.
- Prefer `visible: true` for element checks to avoid testing hidden state.
- Clean up database state using `DatabaseCleaner` if not using system test transaction defaults.

## Mocking & Stubbing
- Use `instance_double` or `class_double` for verifying doubles.
- Only mock what you don't own (e.g., external APIs).

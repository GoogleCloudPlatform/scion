# Rails Idioms & Patterns

## Service Objects
Use Service Objects for complex business logic that doesn't fit naturally in a model or controller.

- **Convention**: Place in `app/services/`.
- **Pattern**: A single public `call` method.
- **Example**:
  ```ruby
  class ProcessPayment
    def initialize(user, amount)
      @user = user
      @amount = amount
    end

    def call
      # logic here
    end
  end
  ```

## Concerns
Use Concerns to extract shared logic across models or controllers.

- **Convention**: Place in `app/models/concerns/` or `app/controllers/concerns/`.
- **Pattern**: Use `ActiveSupport::Concern`.
- **Tip**: Avoid overusing concerns for organization only; use them for true shared behavior.

## Query Objects
For complex ActiveRecord queries, extract them into dedicated Query Objects.

- **Convention**: Place in `app/queries/`.
- **Benefit**: Keeps models lean and makes queries testable in isolation.

## ViewComponents
For complex UI logic, prefer `ViewComponent` over partials or helper methods.
- **Benefit**: Encapsulation, performance, and easier testing.

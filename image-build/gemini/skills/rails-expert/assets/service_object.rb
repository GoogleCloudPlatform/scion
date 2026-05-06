# frozen_string_literal: true

class ServiceTemplate
  def self.call(...)
    new(...).call
  end

  def initialize(...)
    # Initialize your service
  end

  def call
    # Implement your business logic
    # Return a Result object or value
  end
end

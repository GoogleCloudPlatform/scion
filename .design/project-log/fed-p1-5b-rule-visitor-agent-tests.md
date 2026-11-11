# Federated project access item 5B: tests for rule-created visitors on the agent list and read routes

Date: 2026-10-17. Fork PR ptone/scion#4930, tracked in ptone/scion#4603.

- Test-only change in `pkg/hub/federation_visitor_agents_test.go`. Two tests cover a visitor created by a domain rule on the agent list and read routes and the own grants and DM routes: `TestVisitorAgents_RuleVisitorAdmittedUntilRuleRemoved` (removing the rule refuses list and read on the next request and empties both own lists) and `TestVisitorAgents_RuleVisitorRevokedStaysRefused` (removing the visitor does the same, and the rule does not bind the account again on later requests). No production code changed; the binding scan already treats rule grants like grants by email.

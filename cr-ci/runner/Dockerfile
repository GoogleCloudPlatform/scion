# TODO(P3): placeholder, not buildable into a working runner yet.
#
# P3 replaces this with the real runner image (see cr-ci/README.md, "Phases"):
#   - base ubuntu:24.04; GitHub Actions runner, Go 1.26.1, golangci-lint v2.11.4,
#     git, jq, curl, ca-certificates; tool versions pinned in ONE place
#   - non-root runner user; entrypoint.sh as the entrypoint
#   - no Docker daemon (Cloud Run runs in gVisor; DinD and `services:` do not work)
# Design: gs://scion-xproject-exchange/cr-runners/design.md, sections 1.2 and 7.
FROM ubuntu:24.04
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

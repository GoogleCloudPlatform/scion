# Test image for container_test.go. The scion user gets unrestricted
# passwordless sudo on purpose: the test checks that even full sudo cannot
# change the nat table once sciontool init has finished network setup.
#
# The base is pinned by digest, so a mutable tag never decides what the
# test runs on. PACKAGES lets the test build a variant without iptables
# (the missing-tools refusal case).
ARG BASE=debian:stable-slim@sha256:13e0a2e78aa0554ad5ff66e4e260c6dbcdff807eb1f39d43b9fb0288aae88b04
FROM ${BASE}
ARG PACKAGES="iptables sudo procps"
RUN apt-get update \
  && apt-get install -y --no-install-recommends ${PACKAGES} \
  && rm -rf /var/lib/apt/lists/* \
  && useradd -m -u 1000 -s /bin/sh scion \
  && echo "scion ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/scion-test \
  && chmod 0440 /etc/sudoers.d/scion-test
COPY harness.sh /usr/local/bin/netcap-harness.sh
RUN chmod 0755 /usr/local/bin/netcap-harness.sh

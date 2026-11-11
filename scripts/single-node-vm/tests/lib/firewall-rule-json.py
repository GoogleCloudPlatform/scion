#!/usr/bin/env python3
"""tests/lib/firewall-rule-json.py — builds a fake firewall-rule JSON body,
in the same shape `gcloud compute firewall-rules describe --format=json`
returns, from plain positional fields. Shared by the stub gcloud's
`create` handler and by the test harness's fixture-seeding helper, so a
rule the stub "creates" and a rule a test seeds directly are described
identically to hybrid-tier.sh's parser. Never contacts GCP.

Usage:
  firewall-rule-json.py DESC NETWORK DIRECTION ACTION PROTO PORTS \
      SOURCE_TAGS SOURCE_RANGES TARGET_TAGS PRIORITY \
      [SOURCE_SAS [TARGET_SAS [DEST_RANGES [DISABLED [EXTRA_PROTO [EXTRA_PORTS]]]]]]

The first 10 arguments are required. PROTO/PORTS/SOURCE_TAGS/
SOURCE_RANGES/TARGET_TAGS/SOURCE_SAS/TARGET_SAS/DEST_RANGES/EXTRA_PORTS
are comma-separated (an empty string means "not set"). DISABLED is
"true" or "false" (default "false" when omitted). EXTRA_PROTO/
EXTRA_PORTS, when given, add a *second* allowed/denied entry alongside
the first -- used only to build the "more than one rule entry" drift
fixture; every other caller omits them. Prints the JSON body to stdout.
"""
import json
import sys


def csv(s):
    return [x for x in s.split(",") if x]


def main():
    args = list(sys.argv[1:]) + [""] * 16
    (desc, network, direction, action, proto, ports,
     source_tags, source_ranges, target_tags, priority,
     source_sas, target_sas, dest_ranges, disabled,
     extra_proto, extra_ports) = args[:16]

    body = {
        "description": desc,
        "network": f"https://www.googleapis.com/compute/v1/projects/x/global/networks/{network}",
        "direction": direction,
        "targetTags": csv(target_tags),
        "priority": int(priority) if priority else None,
        "disabled": disabled == "true",
    }

    rules = [{"IPProtocol": proto}]
    port_list = csv(ports)
    if port_list:
        rules[0]["ports"] = port_list
    if extra_proto:
        extra_rule = {"IPProtocol": extra_proto}
        extra_port_list = csv(extra_ports)
        if extra_port_list:
            extra_rule["ports"] = extra_port_list
        rules.append(extra_rule)

    if action == "ALLOW":
        body["allowed"] = rules
    elif action == "DENY":
        body["denied"] = rules

    if source_tags:
        body["sourceTags"] = csv(source_tags)
    if source_ranges:
        body["sourceRanges"] = csv(source_ranges)
    if source_sas:
        body["sourceServiceAccounts"] = csv(source_sas)
    if target_sas:
        body["targetServiceAccounts"] = csv(target_sas)
    if dest_ranges:
        body["destinationRanges"] = csv(dest_ranges)

    json.dump(body, sys.stdout)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""tests/lib/k8s-manifest-to-json.py — turns one of this repo's own
rendered PV/Namespace/PVC manifests (read from stdin) into the minimal
JSON shape the stub kubectl stores and serves back for `get ... -o
json`.  Not a general YAML parser: it knows exactly the fixed
indentation hybrid-tier.sh's own render functions produce, and extracts
only the fields the hybrid tier's object-management logic reads.
"""
import sys
import json


def field(lines, indent, key):
    prefix = " " * indent + key + ":"
    for line in lines:
        if line.startswith(prefix):
            return line[len(prefix):].strip().strip('"')
    return None


def main():
    text = sys.stdin.read()
    lines = text.split("\n")

    kind = field(lines, 0, "kind")
    name = field(lines, 2, "name")
    namespace = field(lines, 2, "namespace")  # metadata.namespace (PVC only)
    label = field(lines, 4, "scion-deployment")
    reclaim = field(lines, 2, "persistentVolumeReclaimPolicy")
    volume_name = field(lines, 2, "volumeName")
    nfs_server = field(lines, 4, "server")
    nfs_path = field(lines, 4, "path")
    claimref_ns = field(lines, 4, "namespace")  # spec.claimRef.namespace (PV only)
    claimref_name = field(lines, 4, "name")     # spec.claimRef.name (PV only)

    obj = {
        "kind": kind,
        "metadata": {
            "name": name,
            "labels": ({"scion-deployment": label} if label else {}),
        },
        "spec": {},
    }
    if namespace and kind != "PersistentVolume":
        obj["metadata"]["namespace"] = namespace
    if reclaim:
        obj["spec"]["persistentVolumeReclaimPolicy"] = reclaim
    if volume_name:
        obj["spec"]["volumeName"] = volume_name
    if nfs_server or nfs_path:
        obj["spec"]["nfs"] = {"server": nfs_server, "path": nfs_path}
    if claimref_ns or claimref_name:
        obj["spec"]["claimRef"] = {"namespace": claimref_ns, "name": claimref_name}

    print(json.dumps(obj))


if __name__ == "__main__":
    main()

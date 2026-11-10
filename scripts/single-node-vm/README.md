# Single-Node VM Deployment Scripts

Deploy a Scion Hub on a GCE VM with IAP proxy authentication.

The deploy wizard prompts for hub name, region, machine size, disk size, chat
plugins, and **container images**. For container images, you can either provide
a registry path (if images are already pushed) or build them locally on the VM
from source (~15 min, ~10 GB disk). Building on the VM uses
`image-build/scripts/build-release-images.sh` from this checkout and needs
release v0.3.0-preview.4 or later. An older release given with `--version` is
refused before anything is created, even if the VM already has its images; to
keep an older release the VM already runs, re-run without `--version` (it is
refused only if its images must be rebuilt), or use registry images.

See the full guide: [`docs/deploy/single-node-vm.md`](../../docs/deploy/single-node-vm.md).

# Single-Node VM Deployment Scripts

Deploy a Scion Hub on a GCE VM with IAP proxy authentication.

The deploy wizard prompts for hub name, region, machine size, disk size, chat
plugins, and **container images**. For container images, you can either provide
a registry path (if images are already pushed) or build them locally on the VM
from source (~15 min, ~10 GB disk).

See the full guide: [`docs/deploy/single-node-vm.md`](../../docs/deploy/single-node-vm.md).

This directory contains a generic starter deployment for running Scion in Kubernetes.

The base deploys a single Scion combo pod in the `scion` namespace with:

- Hub API
- Runtime Broker
- Web dashboard
- SQLite and local template storage on a PVC
- a `ClusterIP` service intended for `kubectl port-forward`
- development auth enabled by default for evaluation and local testing

The base intentionally does not assume any cluster-specific ingress, TLS, telemetry,
network policy, private registry, or node scheduling setup.

## Prerequisites

Scion does not currently publish pre-built images. Build and push the images you
want the cluster to use first.

For a quick start:

```bash
image-build/scripts/build-images.sh \
  --registry ghcr.io/<your-org> \
  --target all \
  --platform linux/amd64 \
  --push
```

If your cluster is multi-arch, use `--platform all` instead.

## Configure the base

1. Set the Scion server image:

   ```bash
   cd hack/k8s
   kustomize edit set image scion-server=ghcr.io/<your-org>/scion-server:latest
   ```

2. Edit `settings.yaml`:
   - set `image_registry` to the registry where your Scion harness images were pushed
   - keep `server.hub.public_url` as `http://127.0.0.1:8080` if you plan to access Scion through `kubectl port-forward`
   - change `server.hub.public_url` and `server.auth.dev_mode` before exposing Scion outside the cluster

3. If you want stable web sessions or OAuth, copy `auth-secret.example.yaml` to
   `auth-secret.yaml`, fill in the values, and apply it before the deployment:

   ```bash
   cp auth-secret.example.yaml auth-secret.yaml
   kubectl apply -f auth-secret.yaml
   ```

The deployment loads `scion-auth` as an optional secret, so the base still works
without it.

## Deploy

```bash
kubectl apply -k hack/k8s
kubectl -n scion rollout status deploy/scion
kubectl -n scion port-forward svc/scion 8080:80
```

Open <http://127.0.0.1:8080>.

With the default `dev_mode: true` setting, the web UI can be used without OAuth.
For CLI access, export the Hub endpoint and the generated dev token:

```bash
export SCION_HUB_ENDPOINT=http://127.0.0.1:8080
export SCION_DEV_TOKEN="$(kubectl -n scion exec deploy/scion -- cat /home/scion/.scion/dev-token)"
```

## Productionizing

Before using this outside a trusted environment:

- set `server.auth.dev_mode: false` in `settings.yaml`
- create `scion-auth` from `auth-secret.example.yaml`
- change `server.hub.public_url` to the external URL that users will visit
- add ingress or Gateway API resources for your cluster
- add image pull secrets if your server or harness images live in a private registry
- add network policies that match your cluster's API server, DNS, and egress rules

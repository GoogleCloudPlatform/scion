# cr-ci mediator image. Build context: the cr-ci/ module root, for example
#   docker build -f mediator/Dockerfile -t <ref> cr-ci
# (scripts/deploy-mediator.sh does this with Cloud Build). The module has no
# dependencies, so the build needs no network beyond the base images.
#
# Base images are pinned by digest (read from the public registries on
# 2026-10-21). Bump the Go image together with go.mod.
ARG GO_IMAGE=golang:1.26.1-bookworm@sha256:ab3d6955bbc813a0f3fdf220c1d817dd89c0b3f283777db8ece4a32fe7858edd
ARG RUN_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM ${GO_IMAGE} AS build
WORKDIR /src
# The build context holds only go.mod and the non-test files of mediator/:
# .gcloudignore (Cloud Build upload) and .dockerignore (local builds) are
# allowlists. So the whole context is copied; nothing else of the repo (and
# no secret or config file) can be in it.
COPY . ./
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local GOPROXY=off
RUN go build -buildvcs=false -ldflags=-s -o /out/mediator ./mediator

FROM ${RUN_IMAGE}
COPY --from=build /out/mediator /mediator
USER nonroot:nonroot
ENTRYPOINT ["/mediator"]

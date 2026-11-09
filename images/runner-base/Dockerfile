# syntax=docker/dockerfile:1
#
# The base image for execution environments: a small Debian with the
# dispatch-runner as its entrypoint, and dispatch-ee-install for adding
# packages in images built on it. Most EEs start from a starter EE
# (ee-shell, ee-python, ee-ansible) instead; see examples/ee-template.
#
# The `runner` target is an image holding only /dispatch-runner, for copying
# the runner into an image built on some other base:
#
#   COPY --from=dispatch-ops/runner:<version> /dispatch-runner /usr/local/bin/dispatch-runner
#
# Build context: the repository root (`make images`).

# The runner is cross-compiled on the build platform, so multi-platform
# builds don't run Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/dispatch-runner ./cmd/dispatch-runner
COPY internal ./internal
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dispatch-runner ./cmd/dispatch-runner

FROM scratch AS runner
COPY --from=build /out/dispatch-runner /dispatch-runner
ENTRYPOINT ["/dispatch-runner"]

# The last stage is the default target: runner-base.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS runner-base
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home --home-dir /home/runner --shell /bin/bash runner
COPY --from=build /out/dispatch-runner /usr/local/bin/dispatch-runner
COPY --chmod=0755 images/runner-base/dispatch-ee-install /usr/local/bin/dispatch-ee-install
# Job pods run as non-root, so EE images must name a numeric user.
USER 1000:1000
WORKDIR /runner
ENTRYPOINT ["/usr/local/bin/dispatch-runner"]

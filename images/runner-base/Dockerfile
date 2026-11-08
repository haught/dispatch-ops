# syntax=docker/dockerfile:1
#
# The base image for execution environments: a small Debian with the
# dispatch-runner as its entrypoint. Build your own EE with
#
#   FROM dispatch-ops/runner-base:<version>
#   USER root
#   RUN apt-get update && apt-get install -y --no-install-recommends <tools>
#   USER 1000:1000
#
# Build context: the repository root (`make images`).

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/dispatch-runner ./cmd/dispatch-runner
COPY internal ./internal
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dispatch-runner ./cmd/dispatch-runner

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home --home-dir /home/runner --shell /bin/bash runner
COPY --from=build /out/dispatch-runner /usr/local/bin/dispatch-runner
# Job pods run as non-root, so EE images must name a numeric user.
USER 1000:1000
WORKDIR /runner
ENTRYPOINT ["/usr/local/bin/dispatch-runner"]

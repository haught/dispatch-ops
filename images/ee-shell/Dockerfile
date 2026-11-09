# syntax=docker/dockerfile:1
#
# Starter execution environment for shell jobs: bash, curl, jq, git and an
# SSH client on top of the runner base image. Add Debian packages with
# dispatch-ee-install (see examples/ee-template):
#
#   FROM dispatch-ops/ee-shell:<version>
#   USER root
#   COPY packages.txt /tmp/ee/
#   RUN dispatch-ee-install /tmp/ee && rm -rf /tmp/ee
#   USER 1000:1000

ARG RUNNER_BASE=dispatch-ops/runner-base:dev
FROM ${RUNNER_BASE}
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash curl jq git openssh-client \
    && rm -rf /var/lib/apt/lists/*
USER 1000:1000

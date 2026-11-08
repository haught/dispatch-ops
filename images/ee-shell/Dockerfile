# syntax=docker/dockerfile:1
#
# Starter execution environment for shell jobs: bash, curl, jq, git and an
# SSH client on top of the runner base image.

ARG RUNNER_BASE=dispatch-ops/runner-base:dev
FROM ${RUNNER_BASE}
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash curl jq git openssh-client \
    && rm -rf /var/lib/apt/lists/*
USER 1000:1000

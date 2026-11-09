# syntax=docker/dockerfile:1
#
# Starter execution environment for Python scripts: Python 3 in a virtual
# environment on PATH (with requests and PyYAML), git and an SSH client, on
# top of the runner base image. Add Debian and Python packages with
# dispatch-ee-install (see examples/ee-template):
#
#   FROM dispatch-ops/ee-python:<version>
#   USER root
#   COPY packages.txt requirements.txt /tmp/ee/
#   RUN dispatch-ee-install /tmp/ee && rm -rf /tmp/ee
#   USER 1000:1000

ARG RUNNER_BASE=dispatch-ops/runner-base:dev
FROM ${RUNNER_BASE}
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-venv git openssh-client \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m venv /opt/venv \
    && /opt/venv/bin/pip install --no-cache-dir requests pyyaml \
    && chown -R 1000:1000 /opt/venv
ENV PATH=/opt/venv/bin:$PATH \
    DISPATCH_EE_PIP=/opt/venv/bin/pip
USER 1000:1000

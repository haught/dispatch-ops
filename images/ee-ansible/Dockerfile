# syntax=docker/dockerfile:1
#
# Starter execution environment for Ansible jobs and inventory syncs:
# ansible-core, the requests and netaddr libraries inventory plugins often
# need, and a few common collections, plus git, an SSH client and sshpass (for
# username/password machine credentials), on top of the runner base image.
#
# Add your own collections and Python packages with
#
#   FROM dispatch-ops/ee-ansible:<version>
#   USER root
#   RUN ansible-galaxy collection install -p /usr/share/ansible/collections <collection>
#   USER 1000:1000

ARG RUNNER_BASE=dispatch-ops/runner-base:dev
FROM ${RUNNER_BASE}
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-venv git openssh-client sshpass \
    && rm -rf /var/lib/apt/lists/*
ARG ANSIBLE_CORE_VERSION=2.22.0
RUN python3 -m venv /opt/ansible \
    && /opt/ansible/bin/pip install --no-cache-dir "ansible-core==${ANSIBLE_CORE_VERSION}" requests netaddr \
    && /opt/ansible/bin/ansible-galaxy collection install -p /usr/share/ansible/collections \
        ansible.posix community.general ansible.utils
ENV PATH=/opt/ansible/bin:$PATH
USER 1000:1000

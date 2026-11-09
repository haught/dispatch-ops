# syntax=docker/dockerfile:1
#
# Starter execution environment for Ansible jobs and inventory syncs:
# ansible-core, the requests and netaddr libraries inventory plugins often
# need, and a few common collections, plus git, an SSH client and sshpass (for
# username/password machine credentials), on top of the runner base image.
#
# Add your own Debian packages, Python packages and collections with
# dispatch-ee-install (see examples/ee-template):
#
#   FROM dispatch-ops/ee-ansible:<version>
#   USER root
#   COPY packages.txt requirements.txt requirements.yml /tmp/ee/
#   RUN dispatch-ee-install /tmp/ee && rm -rf /tmp/ee
#   USER 1000:1000

ARG RUNNER_BASE=dispatch-ops/runner-base:dev
FROM ${RUNNER_BASE}
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-venv git openssh-client sshpass \
    && rm -rf /var/lib/apt/lists/*
# Pinned, so a rebuild gives the same image; bump these deliberately.
ARG ANSIBLE_CORE_VERSION=2.22.0
ARG REQUESTS_VERSION=2.34.2
ARG NETADDR_VERSION=1.3.0
ARG ANSIBLE_POSIX_VERSION=2.2.2
ARG COMMUNITY_GENERAL_VERSION=14.0.0
ARG ANSIBLE_UTILS_VERSION=6.1.1
RUN python3 -m venv /opt/ansible \
    && /opt/ansible/bin/pip install --no-cache-dir "ansible-core==${ANSIBLE_CORE_VERSION}" \
        "requests==${REQUESTS_VERSION}" "netaddr==${NETADDR_VERSION}" \
    && /opt/ansible/bin/ansible-galaxy collection install -p /usr/share/ansible/collections \
        "ansible.posix:==${ANSIBLE_POSIX_VERSION}" "community.general:==${COMMUNITY_GENERAL_VERSION}" \
        "ansible.utils:==${ANSIBLE_UTILS_VERSION}"
ENV PATH=/opt/ansible/bin:$PATH \
    DISPATCH_EE_PIP=/opt/ansible/bin/pip \
    DISPATCH_EE_COLLECTIONS=/usr/share/ansible/collections \
    DISPATCH_EE_ROLES=/usr/share/ansible/roles
USER 1000:1000

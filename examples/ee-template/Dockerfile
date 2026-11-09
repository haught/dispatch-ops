# syntax=docker/dockerfile:1
#
# A Dispatch Ops execution environment: a starter EE plus whatever
# packages.txt, requirements.txt and requirements.yml list. See README.md.

# The starter EE to build on: ee-ansible, ee-python or ee-shell. IMAGE_REPO
# is where Dispatch Ops releases publish them (change it for a mirror); pin
# IMAGE_TAG to the release your server runs. `make images` in a Dispatch Ops
# checkout overrides both (IMAGE_REPO=dispatch-ops, IMAGE_TAG=dev) when this
# directory is linked into images/local.
ARG IMAGE_REPO=ghcr.io/haught/dispatch-ops
# Renovate bumps the tag below; keep depName in step with IMAGE_REPO and the
# starter EE named in FROM.
# renovate: datasource=docker depName=ghcr.io/haught/dispatch-ops/ee-ansible
ARG IMAGE_TAG=latest
FROM ${IMAGE_REPO}/ee-ansible:${IMAGE_TAG}

USER root
# Keep all three files, even when one lists nothing. To install Ansible content
# from roles/requirements.yml or collections/requirements.yml as well, copy
# them to the same relative paths under /tmp/ee.
COPY packages.txt requirements.txt requirements.yml /tmp/ee/
RUN dispatch-ee-install /tmp/ee && rm -rf /tmp/ee

# Anything else your jobs need goes here, as root: CA certificates, config
# files, tools from outside Debian.

# Job pods run as a numeric non-root user.
USER 1000:1000

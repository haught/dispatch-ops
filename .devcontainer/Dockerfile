# Dev container base. Go, Node, the Docker CLI and kubectl come from the
# features in devcontainer.json; this adds the remaining OS packages.
FROM mcr.microsoft.com/devcontainers/base:trixie

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        make \
        postgresql-client \
        python3 \
        python3-venv \
        openssh-client \
        sshpass \
    && rm -rf /var/lib/apt/lists/*

# ansible-playbook for the playbook test in `make test-db`. Keep the version in
# step with images/ee-ansible/Dockerfile.
ARG ANSIBLE_CORE_VERSION=2.22.0
RUN python3 -m venv /opt/ansible \
    && /opt/ansible/bin/pip install --no-cache-dir "ansible-core==${ANSIBLE_CORE_VERSION}"
ENV PATH=/opt/ansible/bin:$PATH

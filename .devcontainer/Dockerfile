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

# The development loop (Tiltfile, `make dev`): tilt, the kind CLI it uses to
# load images into the cluster, and helm, which renders the operator's chart.
# Keep KIND_VERSION in step with the Makefile.
ARG TILT_VERSION=0.37.8
ARG KIND_VERSION=v0.33.0
ARG HELM_VERSION=v3.22.1
RUN arch="$(dpkg --print-architecture)" \
    && curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${arch}.tar.gz" \
        | tar -xz -C /usr/local/bin --strip-components=1 "linux-${arch}/helm" \
    && curl -fsSL "https://github.com/tilt-dev/tilt/releases/download/v${TILT_VERSION}/tilt.${TILT_VERSION}.linux.$(echo "$arch" | sed 's/amd64/x86_64/').tar.gz" \
        | tar -xz -C /usr/local/bin tilt \
    && curl -fsSLo /usr/local/bin/kind "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-${arch}" \
    && chmod +x /usr/local/bin/kind

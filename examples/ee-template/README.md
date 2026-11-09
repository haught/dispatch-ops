# My execution environment

A Dispatch Ops execution environment (EE): the image that jobs run in. It is a starter EE
(`ee-ansible`, unless you change the `FROM` line) plus whatever these files
list:

| File | What goes in it |
|---|---|
| `packages.txt` | Debian packages, one per line |
| `requirements.txt` | Python packages, in pip's format |
| `requirements.yml` | Ansible collections and roles, in ansible-galaxy's format |

`dispatch-ee-install`, which ships in every starter EE, installs them. It knows
where the image keeps its Python environment and collections, so you don't
need to. For anything else, add `RUN` steps to the `Dockerfile` before its final
`USER 1000:1000` line.

## Set up

1. Create a repository from this directory.
2. In `Dockerfile`, set `IMAGE_REPO` to where your Dispatch Ops images are
   published, and pin `IMAGE_TAG` to the release your server runs. Update the
   `# renovate:` line to match.
3. Edit the three files, and keep them even when they list nothing.
4. Push. `.github/workflows/build.yml` builds the image, runs
   `dispatch-runner selftest` in it, and pushes it to
   `ghcr.io/<owner>/<repo>`. Pull requests are only built and tested.
5. Turn on [Renovate](https://docs.renovatebot.com/) so the repository gets a
   pull request whenever a new starter EE is released. Rebuilding picks up the
   new runner, so the image keeps working when the server moves to a newer runner protocol.

## Build and check locally

```sh
docker build -t my-ee .
docker run --rm my-ee selftest
```

`selftest` checks that the image runs as a non-root user and has the tools
jobs need (git, ssh, and for Ansible images `ansible-playbook`,
`ansible-inventory` and `ansible-galaxy`). It lists the installed collections
and prints the runner protocol. It exits non-zero when the image can't run
jobs.

To try the image against an unreleased Dispatch Ops, link this checkout into
`images/local/` of a Dispatch Ops checkout and run `make kind-load`. That
builds it on the starter EE from source as `dispatch-ops/local-<name>:dev`.

## Register it

In Dispatch Ops, open **Execution environments** and fill in the **New
execution environment** form with the image `ghcr.io/<owner>/<repo>:<tag>`.
You can also use the API:

```sh
curl -s https://dispatch.example.com/api/v1/execution-environments \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"name": "my-ee", "image": "ghcr.io/<owner>/<repo>:sha-1a2b3c4", "pullPolicy": "IfNotPresent"}'
```

If the package is private, create a **Container registry** credential (server
`ghcr.io`, a username, and a token that can read packages) and choose it as
the EE's registry credential. Dispatch Ops then creates the pull secret for
each job.

Register an immutable tag (`sha-…` or a release like `1.2.3`) with
`IfNotPresent`. Then every job runs the image you tested, and a job's relaunch
runs the same image. Use `latest` with pull policy `Always` only while
iterating.

# Local execution environments

Everything in this directory except this README is gitignored. `make images`
and `make kind-load` build each `images/local/<name>/` that holds a
`Dockerfile` as `dispatch-ops/local-<name>:dev`, after the starter EEs. Tilt
runs `make kind-load` when anything under `images/` changes.

This is for trying an EE against the runner in your checkout before you push
it. Keep the EE itself in its own repository, where it has history, review
and a CI build that pushes it to a registry your cluster can pull from. Start
that repository from [`examples/ee-template`](../../examples/ee-template).
Then link a checkout of it here:

```sh
ln -s ~/src/my-ee images/local/my-ee
make kind-load        # builds dispatch-ops/local-my-ee:dev
```

The build passes `IMAGE_REPO=dispatch-ops` and `IMAGE_TAG=dev`, so a
Dockerfile that starts `FROM ${IMAGE_REPO}/ee-ansible:${IMAGE_TAG}`, as the
template's does, builds on the starter EE you just built.

Register the image as an execution environment with pull policy `Never` or
`IfNotPresent`, because kind already has it loaded.

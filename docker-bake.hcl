# Every Dispatch Ops image, for `docker buildx bake`. Local builds keep the
# dispatch-ops/<name>:dev names; releases set REGISTRY, TAGS and PLATFORMS:
#
#   docker buildx bake                        # all images, native platform
#   docker buildx bake ees --load             # runner-base and the starter EEs
#   REGISTRY=ghcr.io/haught/dispatch-ops TAGS=1.4.0,1.4,latest \
#     PLATFORMS=linux/amd64,linux/arm64 docker buildx bake --push
#
# The EEs build on the runner-base target of the same bake, so nothing has to
# be pushed first.

variable "REGISTRY" {
  default    = "dispatch-ops"
}

# Comma-separated image tags.
variable "TAGS" {
  default    = "dev"
}

# The version built into the binaries (main.version).
variable "VERSION" {
  default    = "dev"
}

# Comma-separated platforms; empty builds for the native platform only.
variable "PLATFORMS" {
  default    = ""
}

variable "REVISION" {
  default    = ""
}

variable "SOURCE" {
  default    = "https://github.com/haught/dispatch-ops"
}

# Set in GitHub Actions to cache layers there, one cache scope per image.
variable "GHA_CACHE" {
  default    = ""
}

function "tags" {
  params     = [name]
  result     = [for t in split(",", TAGS) : "${REGISTRY}/${name}:${trimspace(t)}"]
}

function "cache_from" {
  params     = [name]
  result     = GHA_CACHE == "" ? [] : ["type=gha,scope=${name}"]
}

function "cache_to" {
  params     = [name]
  result     = GHA_CACHE == "" ? [] : ["type=gha,scope=${name},mode=max"]
}

group "default" {
  targets    = ["server", "operator", "runner", "runner-base", "ee-shell", "ee-python", "ee-ansible"]
}

group "ees" {
  targets    = ["runner-base", "ee-shell", "ee-python", "ee-ansible"]
}

target "_common" {
  platforms  = PLATFORMS == "" ? [] : split(",", PLATFORMS)
  args = {
    VERSION = VERSION
  }
  labels = {
    "org.opencontainers.image.source"   = SOURCE
    "org.opencontainers.image.version"  = VERSION
    "org.opencontainers.image.revision" = REVISION
    "org.opencontainers.image.licenses" = "GPL-2.0-only"
  }
}

target "server" {
  inherits   = ["_common"]
  context    = "."
  tags       = tags("server")
  cache-from = cache_from("server")
  cache-to   = cache_to("server")
  labels = {
    "org.opencontainers.image.title"       = "dispatch-ops"
    "org.opencontainers.image.description" = "Dispatch Ops API server and task manager"
  }
}

target "operator" {
  inherits   = ["_common"]
  context    = "."
  target     = "operator"
  tags       = tags("operator")
  cache-from = cache_from("operator")
  cache-to   = cache_to("operator")
  labels = {
    "org.opencontainers.image.title"       = "dispatch-ops-operator"
    "org.opencontainers.image.description" = "Kubernetes operator that installs and upgrades Dispatch Ops"
  }
}

target "runner" {
  inherits   = ["_common"]
  context    = "."
  dockerfile = "images/runner-base/Dockerfile"
  target     = "runner"
  tags       = tags("runner")
  cache-from = cache_from("runner")
  cache-to   = cache_to("runner")
  labels = {
    "org.opencontainers.image.title"       = "dispatch-runner"
    "org.opencontainers.image.description" = "The dispatch-runner binary alone, to copy into custom execution environments"
  }
}

target "runner-base" {
  inherits   = ["_common"]
  context    = "."
  dockerfile = "images/runner-base/Dockerfile"
  target     = "runner-base"
  tags       = tags("runner-base")
  cache-from = cache_from("runner-base")
  cache-to   = cache_to("runner-base")
  labels = {
    "org.opencontainers.image.title"       = "dispatch-ops runner-base"
    "org.opencontainers.image.description" = "Base image for Dispatch Ops execution environments"
  }
}

target "ee" {
  name       = ee
  inherits   = ["_common"]
  matrix = {
    ee = ["ee-shell", "ee-python", "ee-ansible"]
  }
  context    = "images/${ee}"
  args = {
    RUNNER_BASE = "runner-base"
  }
  contexts = {
    "runner-base" = "target:runner-base"
  }
  tags       = tags(ee)
  cache-from = cache_from(ee)
  cache-to   = cache_to(ee)
  labels = {
    "org.opencontainers.image.title"       = "dispatch-ops ${ee}"
    "org.opencontainers.image.description" = "Dispatch Ops starter execution environment"
  }
}

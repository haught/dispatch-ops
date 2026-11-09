// Package joblabels holds the names the task manager gives the Kubernetes
// objects it creates for jobs, and that the operator selects them by (the
// job pods' NetworkPolicy, the task manager's Lease). It has no
// dependencies, so the operator can import it without the task manager.
package joblabels

import (
	"crypto/sha256"
	"encoding/hex"
)

// Labels on job pods, their Jobs and their Secrets.
const (
	ManagedBy = "app.kubernetes.io/managed-by"
	Component = "app.kubernetes.io/component"
	JobID     = "dispatch-ops/job-id"
	// Installation identifies the Dispatch Ops installation (for the
	// operator, the DispatchOps resource's UID) that created the object.
	// Objects created before it existed don't have it.
	Installation = "dispatch-ops.io/installation"

	// ManagedByValue and ComponentValue are the ManagedBy and Component
	// values of job objects.
	ManagedByValue = "dispatch-ops"
	ComponentValue = "job"
)

// InstallationEnv is the task manager's environment variable that holds its
// Installation label value.
const InstallationEnv = "DISPATCH_INSTALLATION"

// leasePrefix is the task manager's Lease, and the whole name for a task
// manager without an installation (run outside the operator).
const leasePrefix = "dispatch-ops-task-manager"

// LeaseName is the leader-election Lease of an installation's task
// managers: one per installation, so installations never compete for one.
func LeaseName(installation string) string {
	if installation == "" {
		return leasePrefix
	}
	sum := sha256.Sum256([]byte(installation))
	return leasePrefix + "-" + hex.EncodeToString(sum[:])[:10]
}

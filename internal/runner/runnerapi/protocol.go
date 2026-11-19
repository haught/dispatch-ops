package runnerapi

// ProtocolVersion is the runner protocol this runner speaks. The API server
// rejects runners whose protocol it doesn't support (see
// api.MinRunnerProtocol and api.MaxRunnerProtocol).
//
// 2: projects, and the ansible and script job types.
// 3: the inventory_sync job type and its inventory report.
// 4: structured Ansible events carry the output Ansible printed for them,
// and host results refer to their task by taskSeq.
const ProtocolVersion = 4

package runnerapi

// ProtocolVersion is the runner protocol this runner speaks. The API server
// rejects runners whose protocol it doesn't support (see
// api.MinRunnerProtocol and api.MaxRunnerProtocol).
//
// 2: projects, and the ansible and script job types.
const ProtocolVersion = 2

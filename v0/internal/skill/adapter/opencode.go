package adapter

// Opencode is a v0.1 stub. Spec §11.Z.4: v0.2 partial implementation
// (commands/agents pass through; hooks need a separate .ts file authored
// per the opencode hook model).
type Opencode struct{}

// Materialise panics — opencode adapter is not implemented in v0.1.
func (Opencode) Materialise(workspaceDir, skillName, skillSourceDir string) error {
	panic("adapter/opencode: not impl in v0.1")
}

// Unmaterialise panics — opencode adapter is not implemented in v0.1.
func (Opencode) Unmaterialise(workspaceDir, skillName string) error {
	panic("adapter/opencode: not impl in v0.1")
}

package codex

import (
	"fmt"

	"github.com/dpemmons/intercom/internal/appserver"
)

// ExecutionPolicy is the launch-scoped command execution boundary for a
// managed Codex service. It is deliberately not persisted in ManagedState;
// omitting --yolo on a later launch returns the thread to workspace-write.
type ExecutionPolicy string

const (
	ExecutionWorkspaceWrite   ExecutionPolicy = "workspace-write"
	ExecutionDangerFullAccess ExecutionPolicy = "danger-full-access"
)

func (p ExecutionPolicy) validate() error {
	switch p {
	case ExecutionWorkspaceWrite, ExecutionDangerFullAccess:
		return nil
	default:
		return fmt.Errorf("codex: unsupported execution policy %q", p)
	}
}

func (p ExecutionPolicy) sandboxMode() appserver.SandboxMode {
	if p == ExecutionDangerFullAccess {
		return appserver.SandboxDangerFullAccess
	}
	return appserver.SandboxWorkspaceWrite
}

func (p ExecutionPolicy) sandboxType() string {
	if p == ExecutionDangerFullAccess {
		return "dangerFullAccess"
	}
	return "workspaceWrite"
}

func (p ExecutionPolicy) IsYolo() bool { return p == ExecutionDangerFullAccess }

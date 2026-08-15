// Package wire is the single source of truth for the OpenRMM protocol on the
// agent side. It mirrors server/src/openrmm/natsio/subjects.py; both are
// written against docs/nats-subjects.md — change that file first.
package wire

import "fmt"

// ProtocolVersion is the envelope version this agent speaks.
const ProtocolVersion = 1

// Agent -> server.

func Heartbeat(agentID string) string { return fmt.Sprintf("agents.%s.heartbeat", agentID) }
func Telemetry(agentID string) string { return fmt.Sprintf("agents.%s.telemetry", agentID) }

func Inventory(agentID, kind string) string {
	return fmt.Sprintf("agents.%s.inventory.%s", agentID, kind)
}

func JobProgress(agentID, jobID string) string {
	return fmt.Sprintf("agents.%s.jobs.%s.progress", agentID, jobID)
}

func JobOutput(agentID, jobID string) string {
	return fmt.Sprintf("agents.%s.jobs.%s.output", agentID, jobID)
}

func JobResult(agentID, jobID string) string {
	return fmt.Sprintf("agents.%s.jobs.%s.result", agentID, jobID)
}

func Events(agentID string) string { return fmt.Sprintf("agents.%s.events", agentID) }

func ShellOut(agentID, sessionID string) string {
	return fmt.Sprintf("agents.%s.shell.%s.out", agentID, sessionID)
}

func ShellCtl(agentID, sessionID string) string {
	return fmt.Sprintf("agents.%s.shell.%s.ctl", agentID, sessionID)
}

// Server -> agent.

func JobsQueue(agentID string) string { return fmt.Sprintf("jobs.%s", agentID) }
func Cmd(agentID, op string) string   { return fmt.Sprintf("cmd.%s.%s", agentID, op) }

// CmdWildcard is what the agent subscribes to for all commands.
func CmdWildcard(agentID string) string { return fmt.Sprintf("cmd.%s.>", agentID) }

func ShellIn(agentID, sessionID string) string {
	return fmt.Sprintf("agents.%s.shell.%s.in", agentID, sessionID)
}

func ShellResize(agentID, sessionID string) string {
	return fmt.Sprintf("agents.%s.shell.%s.rsz", agentID, sessionID)
}

package wire

import "testing"

// These assertions mirror server/tests/test_smoke.py::test_subject_builders_match_contract.
// If one side changes, both tests must be updated against docs/nats-subjects.md.
func TestSubjectsMatchContract(t *testing.T) {
	cases := map[string]string{
		Heartbeat("x"):             "agents.x.heartbeat",
		Telemetry("x"):             "agents.x.telemetry",
		Inventory("x", "hardware"): "agents.x.inventory.hardware",
		JobOutput("x", "j1"):       "agents.x.jobs.j1.output",
		JobsQueue("x"):             "jobs.x",
		Cmd("x", "shell.open"):     "cmd.x.shell.open",
		CmdWildcard("x"):           "cmd.x.>",
		ShellIn("x", "s1"):         "agents.x.shell.s1.in",
		ShellOut("x", "s1"):        "agents.x.shell.s1.out",
		ShellResize("x", "s1"):     "agents.x.shell.s1.rsz",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("subject mismatch: got %q want %q", got, want)
		}
	}
}

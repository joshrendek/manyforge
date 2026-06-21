package sandbox

import "testing"

func TestFakeRunnerRecordsSpec(t *testing.T) {
	f := &FakeRunner{Result: SandboxResult{ExitCode: 0}}
	_, _ = f.Run(t.Context(), SandboxSpec{Image: "img", Cmd: []string{"echo"}})
	if f.Last.Image != "img" {
		t.Fatalf("spec not recorded")
	}
}

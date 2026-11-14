package agent

import (
	"testing"

	"github.com/tuvo1106/ozymandias/internal/agent/config"
)

// A key read from a file usually ends in a newline, which net/http refuses as a
// header value; the sender would then fail for a reason that looks like the network.
func TestIntakeKey_IsTrimmedAndTheVariableIsConfigurable(t *testing.T) {
	t.Setenv("OZYMANDIAS_API_KEY", "ozy_abc\n")
	t.Setenv("MY_KEY", "  ozy_def \r\n")
	a := &Agent{cfg: config.Default()}
	if got := a.intakeKey(); got != "ozy_abc" {
		t.Fatalf("default variable: %q", got)
	}
	a.cfg.Intake.KeyEnv = "MY_KEY"
	if got := a.intakeKey(); got != "ozy_def" {
		t.Fatalf("named variable: %q", got)
	}
	a.cfg.Intake.KeyEnv = "UNSET_VARIABLE_FOR_TEST"
	if got := a.intakeKey(); got != "" {
		t.Fatalf("an unset variable gave %q", got)
	}
}

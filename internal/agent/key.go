package agent

import (
	"os"
	"strings"
)

// intakeKey returns the API key for ozyd, from the variable the config names
// (OZYMANDIAS_API_KEY by default). The key lives in the environment and never in the
// config file, which gets committed and shipped in images.
func (a *Agent) intakeKey() string {
	name := a.cfg.Intake.KeyEnv
	if name == "" {
		name = "OZYMANDIAS_API_KEY"
	}
	// A key from a secret file or `$(cat file)` often ends in a newline, which
	// net/http rejects as a header value; the operator would be sent to look at
	// connectivity instead of the key.
	return strings.TrimSpace(os.Getenv(name))
}

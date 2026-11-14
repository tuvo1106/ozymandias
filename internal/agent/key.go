package agent

import "os"

// intakeKey returns the API key for ozyd, from the variable the config names
// (OZYMANDIAS_API_KEY by default). The key lives in the environment and never in the
// config file, which gets committed and shipped in images.
func (a *Agent) intakeKey() string {
	name := a.cfg.Intake.KeyEnv
	if name == "" {
		name = "OZYMANDIAS_API_KEY"
	}
	return os.Getenv(name)
}

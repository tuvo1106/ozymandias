package server

import (
	"context"
	"fmt"

	"github.com/tuvo1106/ozymandias/internal/auth"
	"github.com/tuvo1106/ozymandias/internal/meta"
)

// defaultAdminPasswordEnv is where the admin password is read from when the
// config does not name another variable.
const defaultAdminPasswordEnv = "OZYMANDIAS_ADMIN_PASSWORD"

// newAuth builds the gate from the config. It fails when auth is on and the
// password is not set: starting open, with only a log line to say so, would turn
// a missing environment variable into an unprotected server.
func (s *Server) newAuth(md *meta.DB, getenv func(string) string) (*auth.Auth, error) {
	c := s.cfg.Auth
	env := c.AdminPasswordEnv
	if env == "" {
		env = defaultAdminPasswordEnv
	}
	pw := getenv(env)
	if pw == "" {
		return nil, fmt.Errorf("auth.enabled is set but $%s is empty", env)
	}
	keys, err := auth.NewKeyring(context.Background(), md, s.clock, auth.Limits{
		RequestsPerSec: c.Defaults.RequestsPerSec, BytesPerSec: c.Defaults.BytesPerSec,
		SpansPerSec: c.Defaults.SpansPerSec, LogBytesPerDay: c.Defaults.LogBytesPerDay,
	})
	if err != nil {
		return nil, fmt.Errorf("loading api keys: %w", err)
	}
	return auth.New(auth.Options{
		Keys: keys, AdminPassword: pw, SessionTTL: c.SessionTTL,
		Clock: s.clock, Registry: s.reg, Secure: c.SecureCookie,
	})
}

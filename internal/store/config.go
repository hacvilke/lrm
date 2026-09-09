package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SaveConfig persists the (possibly edited) repo config.
func (r *Repo) SaveConfig() error {
	raw, err := json.MarshalIndent(r.Config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.LrmDir, "config.json"), raw, 0o644); err != nil {
		return err
	}
	return nil
}

// SetUser validates and stages a new author name (call SaveConfig after).
func (r *Repo) SetUser(user string) error {
	user = strings.TrimSpace(user)
	if user == "" {
		return fmt.Errorf("user name must not be empty")
	}
	if len(user) > 64 || strings.ContainsAny(user, "\n\r") {
		return fmt.Errorf("user name must be 1..64 chars, single line")
	}
	r.Config.User = user
	return nil
}

// SetPort validates and stages a new default port (call SaveConfig after).
func (r *Repo) SetPort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("port must be 1..65535")
	}
	r.Config.Port = port
	return nil
}

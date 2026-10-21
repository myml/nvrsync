package config

import "testing"

func TestParseResetAuth(t *testing.T) {
	clearEnv(t)

	cfg, err := Parse([]string{"-data", t.TempDir(), "-reset-auth"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.ResetAuth {
		t.Error("ResetAuth = false after -reset-auth")
	}

	cfg, err = Parse([]string{"-data", t.TempDir()})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ResetAuth {
		t.Error("ResetAuth = true without -reset-auth")
	}

	cfg, err = Parse([]string{"-data", t.TempDir(), "-reset-auth=false"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ResetAuth {
		t.Error("ResetAuth = true after -reset-auth=false")
	}
}

package store

import (
	"strings"
	"testing"
)

const testAuthHash = "$2a$10$0123456789012345678901234567890123456789012345678901234567890"

func TestSettingsNeverExposeAuthHash(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)

	if got := s.AuthHash(); got != "" {
		t.Fatalf("AuthHash() = %q on a fresh store, want empty", got)
	}
	if err := s.SetPassword("alice", testAuthHash); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	got := s.Settings()
	if got.AuthHash != "" {
		t.Errorf("Settings().AuthHash = %q, must never expose the hash", got.AuthHash)
	}
	if got.AuthUser != "alice" {
		t.Errorf("Settings().AuthUser = %q, want alice", got.AuthUser)
	}
	if got.AuthEnabled {
		t.Errorf("Settings().AuthEnabled = true on a fresh store")
	}
	hash := s.AuthHash()
	if hash != testAuthHash {
		t.Errorf("AuthHash() = %q, want stored hash", hash)
	}

	// The hash is only allowed on disk.
	onDisk := readConfigFile(t, dir)
	if onDisk.AuthHash != testAuthHash {
		t.Errorf("config.json auth_hash = %q, want the stored hash", onDisk.AuthHash)
	}
	if onDisk.AuthUser != "alice" {
		t.Errorf("config.json auth_user = %q, want alice", onDisk.AuthUser)
	}

	reopened := openStoreAt(t, dir)
	if got := reopened.Settings().AuthHash; got != "" {
		t.Errorf("reopened Settings().AuthHash = %q, want empty", got)
	}
	if got := reopened.AuthHash(); got != testAuthHash {
		t.Errorf("reopened AuthHash() = %q, want stored hash", got)
	}
}

func TestSaveSettingsPreservesAuthHash(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)

	const stored = "stored-hash"
	if err := s.SetPassword("admin", stored); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// This is what the frontend sends back: Settings() has no hash.
	next := s.Settings()
	next.NVRHost = "192.0.2.55"
	next.AuthEnabled = true
	if err := s.SaveSettings(next); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if got := s.AuthHash(); got != stored {
		t.Errorf("AuthHash() = %q after settings round trip, want %q", got, stored)
	}
	if !s.Settings().AuthEnabled {
		t.Errorf("AuthEnabled was not saved")
	}

	// Even a caller that tries to inject a hash cannot replace the stored one;
	// SetPassword is the only way to change it.
	injected := s.Settings()
	injected.AuthHash = "attacker-controlled"
	injected.NVRHost = "192.0.2.56"
	if err := s.SaveSettings(injected); err != nil {
		t.Fatalf("SaveSettings with injected hash: %v", err)
	}
	if got := s.AuthHash(); got != stored {
		t.Errorf("AuthHash() = %q after injected hash, want %q", got, stored)
	}
	onDisk := readConfigFile(t, dir)
	if onDisk.AuthHash != stored {
		t.Errorf("config.json auth_hash = %q, want %q", onDisk.AuthHash, stored)
	}
}

func TestSetPasswordValidation(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetPassword("admin", "  "); err == nil {
		t.Fatal("SetPassword with an empty hash should fail")
	}
	if err := s.SetPassword("   ", testAuthHash); err != nil {
		t.Fatalf("SetPassword with an empty user: %v", err)
	}
	if got := s.Settings().AuthUser; got != DefaultAuthUser {
		t.Errorf("AuthUser = %q, want default %q", got, DefaultAuthUser)
	}
}

func TestResetAuth(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)

	if err := s.SetPassword("keeper", testAuthHash); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	set := s.Settings()
	set.AuthEnabled = true
	if err := s.SaveSettings(set); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	if err := s.ResetAuth(); err != nil {
		t.Fatalf("ResetAuth: %v", err)
	}
	got := s.Settings()
	if got.AuthEnabled {
		t.Error("AuthEnabled still true after ResetAuth")
	}
	if got.AuthHash != "" {
		t.Errorf("Settings().AuthHash = %q after ResetAuth", got.AuthHash)
	}
	if hash := s.AuthHash(); hash != "" {
		t.Errorf("AuthHash() = %q after ResetAuth", hash)
	}
	// The login name is not a secret and is kept as a convenience.
	if got.AuthUser != "keeper" {
		t.Errorf("AuthUser = %q, want keeper to be preserved", got.AuthUser)
	}

	onDisk := readConfigFile(t, dir)
	if onDisk.AuthEnabled || strings.TrimSpace(onDisk.AuthHash) != "" {
		t.Errorf("config.json still has auth enabled/hash: %+v", onDisk)
	}

	reopened := openStoreAt(t, dir)
	if reopened.Settings().AuthEnabled || reopened.AuthHash() != "" {
		t.Error("ResetAuth was not persisted")
	}
}

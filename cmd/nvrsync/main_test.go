package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/myml/nvrsync/internal/store"
)

// TestApplyResetAuth covers the -reset-auth recovery path: it must clear the
// hash and auth_enabled on disk but leave the process able to start normally.
func TestApplyResetAuth(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	hash, err := bcrypt.GenerateFromPassword([]byte("forgotten-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if err := st.SetPassword("admin", string(hash)); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	set := st.Settings()
	set.AuthEnabled = true
	if err := st.SaveSettings(set); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// Without the flag nothing changes.
	if err := applyResetAuth(st, false); err != nil {
		t.Fatalf("applyResetAuth(false): %v", err)
	}
	if !st.Settings().AuthEnabled || st.AuthHash() == "" {
		t.Fatal("applyResetAuth(false) modified the auth settings")
	}

	if err := applyResetAuth(st, true); err != nil {
		t.Fatalf("applyResetAuth(true): %v", err)
	}
	got := st.Settings()
	if got.AuthEnabled {
		t.Error("AuthEnabled is still true after -reset-auth")
	}
	if got.AuthHash != "" || st.AuthHash() != "" {
		t.Errorf("-reset-auth left a hash behind: settings=%q auth=%q", got.AuthHash, st.AuthHash())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var onDisk store.Settings
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("decode config.json: %v", err)
	}
	if onDisk.AuthEnabled || onDisk.AuthHash != "" {
		t.Errorf("config.json still protected after -reset-auth: %+v", onDisk)
	}

	// The reset is persisted, so a restart stays unprotected.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	if reopened.Settings().AuthEnabled || reopened.AuthHash() != "" {
		t.Error("-reset-auth was not persisted")
	}
}

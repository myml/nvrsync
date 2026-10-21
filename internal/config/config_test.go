package config

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	want := Config{DataDir: "./data", Listen: ":8099"}
	if got := Default(); got != want {
		t.Fatalf("Default() = %+v, want %+v", got, want)
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvData, "")
	t.Setenv(EnvListen, "")
}

func TestParseFlags(t *testing.T) {
	clearEnv(t)
	dir := filepath.Join(t.TempDir(), "nested", "data")

	cfg, err := Parse([]string{"-data", dir, "-listen", "127.0.0.1:9000"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.DataDir != dir || cfg.Listen != "127.0.0.1:9000" {
		t.Fatalf("Parse() = %+v", cfg)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("data directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

func TestParseEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvData, dir)
	t.Setenv(EnvListen, ":9100")

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.DataDir != dir || cfg.Listen != ":9100" {
		t.Fatalf("Parse() = %+v, want env values", cfg)
	}
}

func TestParseFlagsOverrideEnv(t *testing.T) {
	envDir := t.TempDir()
	flagDir := filepath.Join(t.TempDir(), "flag-data")
	t.Setenv(EnvData, envDir)
	t.Setenv(EnvListen, ":9200")

	cfg, err := Parse([]string{"-data", flagDir, "-listen", ":9300"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.DataDir != flagDir {
		t.Errorf("DataDir = %q, want flag value %q", cfg.DataDir, flagDir)
	}
	if cfg.Listen != ":9300" {
		t.Errorf("Listen = %q, want flag value :9300", cfg.Listen)
	}
}

func TestParseEmptyDataDir(t *testing.T) {
	clearEnv(t)
	if _, err := Parse([]string{"-data", ""}); err == nil {
		t.Fatal("empty -data should fail")
	}

	t.Setenv(EnvData, "   ")
	if _, err := Parse(nil); err == nil {
		t.Fatal("whitespace -data should fail")
	}
}

func TestParseMkdirError(t *testing.T) {
	clearEnv(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Parse([]string{"-data", filepath.Join(file, "sub")}); err == nil {
		t.Fatal("data directory under a regular file should fail")
	}
}

func TestParseUnknownFlagAndArgs(t *testing.T) {
	clearEnv(t)
	if _, err := Parse([]string{"-nope"}); err == nil {
		t.Fatal("unknown flag should fail")
	}
	if _, err := Parse([]string{"positional"}); err == nil {
		t.Fatal("positional argument should fail")
	}
	if _, err := Parse([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Parse(-h) error = %v, want flag.ErrHelp", err)
	}
}

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvData, dir)
	t.Setenv(EnvListen, ":9400")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != dir || cfg.Listen != ":9400" {
		t.Fatalf("Load() = %+v, want env values", cfg)
	}
}

func TestLoadDefaults(t *testing.T) {
	// Point DataDir at a temp directory so Load's validation does not create
	// ./data relative to the package directory; Listen should fall back to the
	// built-in default.
	dir := t.TempDir()
	t.Setenv(EnvData, dir)
	t.Setenv(EnvListen, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != dir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, dir)
	}
	if cfg.Listen != defaultListen {
		t.Errorf("Listen = %q, want default %q", cfg.Listen, defaultListen)
	}
}

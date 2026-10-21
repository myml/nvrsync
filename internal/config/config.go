// Package config loads and validates the process-wide configuration for
// nvrsync.
//
// Values are resolved with the following precedence, highest first:
//
//	command line flag  >  environment variable  >  built-in default
//
// The two supported flags are -data (DataDir) and -listen (Listen), and the
// matching environment variables are NVRSYNC_DATA and NVRSYNC_LISTEN.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// EnvData and EnvListen are the environment variables used when the
	// corresponding flag is not supplied.
	EnvData   = "NVRSYNC_DATA"
	EnvListen = "NVRSYNC_LISTEN"

	defaultDataDir = "./data"
	defaultListen  = ":8099"
)

// Config is the process-level configuration. Application settings that can be
// changed at run time live in store.Settings instead.
type Config struct {
	DataDir string `json:"data_dir"`
	Listen  string `json:"listen"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		DataDir: defaultDataDir,
		Listen:  defaultListen,
	}
}

// Load returns the effective configuration.
//
// If the process has already parsed its command-line flags (the usual pattern
// in cmd/nvrsync and in tests), flags named "data" and "listen" that were
// registered on the default flag set are honored. Otherwise Load parses
// os.Args[1:] itself. In both cases environment variables fill in anything
// that was not set explicitly.
func Load() (Config, error) {
	if flag.Parsed() {
		cfg := fromEnv()
		set := make(map[string]bool)
		flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["data"] {
			if f := flag.Lookup("data"); f != nil && f.Value.String() != "" {
				cfg.DataDir = f.Value.String()
			}
		}
		if set["listen"] {
			if f := flag.Lookup("listen"); f != nil && f.Value.String() != "" {
				cfg.Listen = f.Value.String()
			}
		}
		if err := cfg.validate(); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	return Parse(os.Args[1:])
}

// Parse builds a configuration from args (without the program name), applying
// flag > environment > default precedence and validating the result.
//
// The DataDir is created if it does not exist yet.
func Parse(args []string) (Config, error) {
	cfg := fromEnv()

	fs := flag.NewFlagSet("nvrsync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory (env "+EnvData+")")
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "HTTP listen address (env "+EnvListen+")")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// fromEnv applies environment overrides to the defaults.
func fromEnv() Config {
	cfg := Default()
	if v := os.Getenv(EnvData); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv(EnvListen); v != "" {
		cfg.Listen = v
	}
	return cfg
}

// validate checks the configuration and makes sure DataDir is usable.
func (c Config) validate() error {
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("config: data directory must not be empty")
	}
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return fmt.Errorf("config: create data directory %q: %w", c.DataDir, err)
	}
	return nil
}

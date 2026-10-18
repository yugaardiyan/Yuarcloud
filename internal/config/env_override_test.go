package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A config key that viper has never heard of — absent from the file it read and
// with no SetDefault — never reaches Unmarshal, so AutomaticEnv drops the
// variable on the floor. These tests pin that down for the eight keys that were
// missing, auth.jwt_secret being the one that locked people out of their own
// server.
//
// They fail on the pre-fix code: the env var is ignored and Load returns a
// Config with the zero value.

func writeAbsentKeyConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvOverrideWorksWhenKeyAbsentFromConfigFile(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  string
		val  string
	}{
		{"jwt_secret", "server:\n  port: 3103\n", "YUAR_AUTH_JWT_SECRET", "a-secret-long-enough-to-pass-the-check-32"},
		{"base_url", "server:\n  port: 3103\n", "YUAR_SHARE_BASE_URL", "https://files.example.com"},
		{"postgres host", "server:\n  port: 3103\n", "YUAR_DATABASE_POSTGRES_HOST", "db.internal"},
		{"postgres dbname", "server:\n  port: 3103\n", "YUAR_DATABASE_POSTGRES_DBNAME", "yuarcloud"},
		{"postgres user", "server:\n  port: 3103\n", "YUAR_DATABASE_POSTGRES_USER", "yuar"},
		{"postgres password", "server:\n  port: 3103\n", "YUAR_DATABASE_POSTGRES_PASSWORD", "s3cret"},
		{"postgres sslmode", "server:\n  port: 3103\n", "YUAR_DATABASE_POSTGRES_SSLMODE", "require"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			cfg, err := Load(writeAbsentKeyConfig(t, tc.file))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var got string
			switch tc.env {
			case "YUAR_AUTH_JWT_SECRET":
				got = cfg.Auth.JWTSecret
			case "YUAR_SHARE_BASE_URL":
				got = cfg.Share.BaseURL
			default:
				got = cfg.Database.Postgres.Host
				switch tc.env {
				case "YUAR_DATABASE_POSTGRES_DBNAME":
					got = cfg.Database.Postgres.DBName
				case "YUAR_DATABASE_POSTGRES_USER":
					got = cfg.Database.Postgres.User
				case "YUAR_DATABASE_POSTGRES_PASSWORD":
					got = cfg.Database.Postgres.Password
				case "YUAR_DATABASE_POSTGRES_SSLMODE":
					got = cfg.Database.Postgres.SSLMode
				}
			}
			if got != tc.val {
				t.Errorf("%s ignored: got %q, want %q", tc.env, got, tc.val)
			}
		})
	}
}

// The exact shape that locked someone out: no config file, only the environment.
func TestEnvOverrideWorksWithNoConfigFileAtAll(t *testing.T) {
	t.Setenv("YUAR_AUTH_JWT_SECRET", "a-secret-long-enough-to-pass-the-check-32")
	t.Setenv("YUAR_SERVER_PORT", "3199")

	empty := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.JWTSecret == "" {
		t.Error("YUAR_AUTH_JWT_SECRET ignored with no config file; server would refuse to start")
	}
	if cfg.Server.Port != 3199 {
		t.Errorf("port: got %d, want 3199", cfg.Server.Port)
	}
}

// A config file that lists the key still works, and the file still wins when no
// env var is set — the fix must not turn the file into decoration.
func TestConfigFileStillWinsWithoutEnvVar(t *testing.T) {
	cfg, err := Load(writeConfig(t, "auth:\n  jwt_secret: \"from-the-file\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.JWTSecret != "from-the-file" {
		t.Errorf("got %q, want from-the-file", cfg.Auth.JWTSecret)
	}
}

func TestEnvStillOverridesConfigFile(t *testing.T) {
	t.Setenv("YUAR_AUTH_JWT_SECRET", "from-the-environment")
	cfg, err := Load(writeConfig(t, "auth:\n  jwt_secret: \"from-the-file\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.JWTSecret != "from-the-environment" {
		t.Errorf("got %q, want from-the-environment", cfg.Auth.JWTSecret)
	}
}

// Absent key must still read as the zero value — the added defaults are a
// no-op for every configuration that shipped before them.
func TestAbsentKeysStillZero(t *testing.T) {
	cfg, err := Load(writeConfig(t, "server:\n  port: 3103\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.JWTSecret != "" {
		t.Errorf("jwt_secret: got %q, want empty", cfg.Auth.JWTSecret)
	}
	if cfg.Share.BaseURL != "" {
		t.Errorf("base_url: got %q, want empty", cfg.Share.BaseURL)
	}
	if cfg.Database.Postgres.Port != 0 {
		t.Errorf("postgres port: got %d, want 0", cfg.Database.Postgres.Port)
	}
}

// Relative paths resolve against the config file's own directory. This is the
// whole reason configs/config.docker.yaml exists: a config at /config/config.yaml
// with a "./data/files" would put user files at /config/data/files, on the state
// volume instead of the files one.
func TestRelativePathsResolveAgainstConfigFileDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("storage:\n  local:\n    root: \"./files\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "files")
	if cfg.Storage.Local.Root != want {
		t.Errorf("root: got %q, want %q", cfg.Storage.Local.Root, want)
	}
}

func TestAbsolutePathsLeftAlone(t *testing.T) {
	cfg, err := Load(writeConfig(t, "storage:\n  local:\n    root: \"/srv\"\n  chunked:\n    temp_dir: \"/srv/.yuarcloud-chunks\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Local.Root != "/srv" {
		t.Errorf("root: got %q, want /srv", cfg.Storage.Local.Root)
	}
	if cfg.Storage.Chunked.TempDir != "/srv/.yuarcloud-chunks" {
		t.Errorf("temp_dir: got %q", cfg.Storage.Chunked.TempDir)
	}
}

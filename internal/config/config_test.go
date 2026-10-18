package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig drops a minimal config on disk and returns its path, so these
// cases exercise the real Load path rather than a hand-built struct.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// Env overrides were decorative for the life of the project: viper looked up
// nested keys as YUAR_STORAGE.LOCAL.ROOT, with dots, which no shell can set.
// These pin the underscore spelling that people actually write.
func TestEnvOverridesNestedKeys(t *testing.T) {
	const body = `
server:
  port: 8080
  mode: debug
storage:
  local:
    root: ./data/files
database:
  sqlite:
    path: ./data/yuarcloud.db
auth:
  jwt_secret: from-file
  token_ttl: 24h
admin:
  default_user: from-file
rate_limit:
  burst: 20
`

	cases := []struct {
		name  string
		env   string
		value string
		check func(*testing.T, *Config)
	}{
		{
			name: "storage.local.root", env: "YUAR_STORAGE_LOCAL_ROOT", value: "/srv",
			check: func(t *testing.T, c *Config) {
				if c.Storage.Local.Root != "/srv" {
					t.Fatalf("storage.local.root = %q, want /srv", c.Storage.Local.Root)
				}
			},
		},
		{
			name: "database.sqlite.path", env: "YUAR_DATABASE_SQLITE_PATH", value: "/config/yuarcloud.db",
			check: func(t *testing.T, c *Config) {
				if c.Database.SQLite.Path != "/config/yuarcloud.db" {
					t.Fatalf("database.sqlite.path = %q", c.Database.SQLite.Path)
				}
			},
		},
		{
			name: "server.port", env: "YUAR_SERVER_PORT", value: "9099",
			check: func(t *testing.T, c *Config) {
				if c.Server.Port != 9099 {
					t.Fatalf("server.port = %d, want 9099", c.Server.Port)
				}
			},
		},
		{
			name: "server.mode", env: "YUAR_SERVER_MODE", value: "release",
			check: func(t *testing.T, c *Config) {
				if c.Server.Mode != "release" {
					t.Fatalf("server.mode = %q, want release (the file says debug)", c.Server.Mode)
				}
			},
		},
		{
			name: "auth.jwt_secret", env: "YUAR_AUTH_JWT_SECRET", value: "from-env",
			check: func(t *testing.T, c *Config) {
				if c.Auth.JWTSecret != "from-env" {
					t.Fatalf("auth.jwt_secret = %q, want from-env (the file says from-file)", c.Auth.JWTSecret)
				}
			},
		},
		{
			name: "auth.token_ttl", env: "YUAR_AUTH_TOKEN_TTL", value: "48h",
			check: func(t *testing.T, c *Config) {
				if c.Auth.TokenTTL != 48*time.Hour {
					t.Fatalf("auth.token_ttl = %v, want 48h", c.Auth.TokenTTL)
				}
			},
		},
		{
			name: "admin.default_user", env: "YUAR_ADMIN_DEFAULT_USER", value: "from-env",
			check: func(t *testing.T, c *Config) {
				if c.Admin.DefaultUser != "from-env" {
					t.Fatalf("admin.default_user = %q", c.Admin.DefaultUser)
				}
			},
		},
		{
			name: "rate_limit.burst", env: "YUAR_RATE_LIMIT_BURST", value: "77",
			check: func(t *testing.T, c *Config) {
				if c.RateLimit.Burst != 77 {
					t.Fatalf("rate_limit.burst = %d, want 77", c.RateLimit.Burst)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			cfg, err := Load(writeConfig(t, body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, cfg)
		})
	}
}

// Relative paths resolve against the config file's directory. That is right for
// a checkout and catastrophic in the image: with the config at
// /app/configs/config.yaml, "./data/files" becomes /app/configs/data/files
// while the volume is mounted at /app/data, so nothing durable is written.
func TestShippedDockerConfigUsesAbsolutePaths(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "config.docker.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("config.docker.yaml not present: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, ":") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "root", "path", "temp_dir", "file_path":
			if value == "" {
				continue // deliberately blank, e.g. base_url
			}
			if !filepath.IsAbs(value) {
				t.Errorf("%s: %q is relative; it would resolve against the config file's directory", strings.TrimSpace(key), value)
			}
		}
	}
}

// The config file the image ships must not carry a signing key. Anyone who can
// read the repository can then mint an admin token for every install that never
// set one.
func TestDockerConfigShipsWithoutAJWTSecret(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "config.docker.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("config.docker.yaml not present: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.JWTSecret != "" {
		t.Fatalf("auth.jwt_secret = %q; the shipped config must leave it empty so the server refuses to start", cfg.Auth.JWTSecret)
	}
}

func TestAbsolutePathsAreLeftAlone(t *testing.T) {
	path := writeConfig(t, `
storage:
  local:
    root: /srv
database:
  sqlite:
    path: /config/yuarcloud.db
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.Local.Root != "/srv" {
		t.Fatalf("storage.local.root = %q, want /srv untouched", cfg.Storage.Local.Root)
	}
	if cfg.Database.SQLite.Path != "/config/yuarcloud.db" {
		t.Fatalf("database.sqlite.path = %q, want /config/yuarcloud.db untouched", cfg.Database.SQLite.Path)
	}
}

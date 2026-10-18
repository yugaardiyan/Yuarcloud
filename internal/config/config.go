package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Storage   StorageConfig   `mapstructure:"storage"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Admin     AdminConfig     `mapstructure:"admin"`
	Share     ShareConfig     `mapstructure:"share"`
	Logging   LoggingConfig   `mapstructure:"logging"`
	RateLimit RateLimitConfig `mapstructure:"rate_limit"`
	CORS      CORSConfig      `mapstructure:"cors"`
}

type ServerConfig struct {
	Host           string        `mapstructure:"host"`
	Port           int           `mapstructure:"port"`
	Mode           string        `mapstructure:"mode"`
	ReadTimeout    time.Duration `mapstructure:"read_timeout"`
	WriteTimeout   time.Duration `mapstructure:"write_timeout"`
	MaxHeaderBytes int           `mapstructure:"max_header_bytes"`
	// TrustedProxies are the CIDRs allowed to set X-Forwarded-For. Empty means
	// none, and that is the safe default: trusting every source (gin's own
	// default) lets any caller choose the address the rate limiter keys on and
	// the one written to the audit log. Operators behind nginx or Cloudflare
	// list their proxy here.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

type DatabaseConfig struct {
	Type     string         `mapstructure:"type"`
	SQLite   SQLiteConfig   `mapstructure:"sqlite"`
	Postgres PostgresConfig `mapstructure:"postgres"`
}

type SQLiteConfig struct {
	Path string `mapstructure:"path"`
}

type PostgresConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	DBName   string `mapstructure:"dbname"`
	SSLMode  string `mapstructure:"sslmode"`
}

type StorageConfig struct {
	Type    string        `mapstructure:"type"`
	Local   LocalConfig   `mapstructure:"local"`
	Chunked ChunkedConfig `mapstructure:"chunked"`
}

type LocalConfig struct {
	Root string `mapstructure:"root"`
}

type ChunkedConfig struct {
	MaxChunkSize    int64         `mapstructure:"max_chunk_size"`
	MaxFileSize     int64         `mapstructure:"max_file_size"`
	TempDir         string        `mapstructure:"temp_dir"`
	CleanupInterval time.Duration `mapstructure:"cleanup_interval"`
}

type AuthConfig struct {
	JWTSecret  string        `mapstructure:"jwt_secret"`
	TokenTTL   time.Duration `mapstructure:"token_ttl"`
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
	BcryptCost int           `mapstructure:"bcrypt_cost"`
	Issuer     string        `mapstructure:"issuer"`
}

type AdminConfig struct {
	DefaultUser string `mapstructure:"default_user"`
	DefaultPass string `mapstructure:"default_pass"`
}

type ShareConfig struct {
	DefaultExpiryDays int    `mapstructure:"default_expiry_days"`
	MaxExpiryDays     int    `mapstructure:"max_expiry_days"`
	RequirePassword   bool   `mapstructure:"require_password"`
	MaxViews          int    `mapstructure:"max_views"`
	BaseURL           string `mapstructure:"base_url"`
}

// LoggingConfig carries one setting, because that is the one thing that is
// read: `debug` turns on gorm's own SQL echo.
//
// `format`, `output` and `file_path` were here and were never read. The
// example config shipped them anyway — including a file_path pointing at
// ./logs/yuarcloud.log, which nothing ever created — so the deployment notes
// promised a log file that did not exist. Logging goes to stderr, which
// `docker logs` and journald already collect.
type LoggingConfig struct {
	Level string `mapstructure:"level"`
}

type RateLimitConfig struct {
	Enabled           bool `mapstructure:"enabled"`
	RequestsPerMinute int  `mapstructure:"requests_per_minute"`
	Burst             int  `mapstructure:"burst"`
}

type CORSConfig struct {
	AllowedOrigins   []string      `mapstructure:"allowed_origins"`
	AllowedMethods   []string      `mapstructure:"allowed_methods"`
	AllowedHeaders   []string      `mapstructure:"allowed_headers"`
	AllowCredentials bool          `mapstructure:"allow_credentials"`
	MaxAge           time.Duration `mapstructure:"max_age"`
}

func Load(configPath string) (*Config, error) {
	v := viper.New()

	// Set defaults
	setDefaults(v)

	// Config file
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath(".")
		v.AddConfigPath("/etc/yuarcloud")
	}

	// Environment variables
	//
	// AutomaticEnv uppercases the key it is given, so a nested key like
	// storage.local.root was looked up as YUAR_STORAGE.LOCAL.ROOT — dots and
	// all. No shell can set that, so *nothing* in the environment had any
	// effect: YUAR_SERVER_PORT left the server on 8080, and YUAR_AUTH_JWTSECRET
	// silently did not change the signing key. The replacer is what turns the
	// dotted key into the underscore form people actually write.
	v.SetEnvPrefix("YUAR")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Read config file (optional)
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
		// Config file not found, use defaults + env
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// Resolve relative paths
	cfg.resolvePaths(configPath)

	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 3103)
	v.SetDefault("server.mode", "release")
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "30s")
	v.SetDefault("server.max_header_bytes", 1048576)
	v.SetDefault("server.trusted_proxies", []string{})

	v.SetDefault("database.type", "sqlite")
	v.SetDefault("database.sqlite.path", "./data/yuarcloud.db")

	v.SetDefault("storage.type", "local")
	v.SetDefault("storage.local.root", "./data/files")
	v.SetDefault("storage.chunked.max_chunk_size", 10485760)
	v.SetDefault("storage.chunked.max_file_size", 10737418240)
	v.SetDefault("storage.chunked.temp_dir", "./data/tmp/chunks")
	v.SetDefault("storage.chunked.cleanup_interval", "1h")

	v.SetDefault("auth.token_ttl", "24h")
	v.SetDefault("auth.refresh_ttl", "168h")
	v.SetDefault("auth.bcrypt_cost", 12)
	v.SetDefault("auth.issuer", "yuarcloud")

	v.SetDefault("admin.default_user", "admin")
	v.SetDefault("admin.default_pass", "admin")

	v.SetDefault("share.default_expiry_days", 7)
	v.SetDefault("share.max_expiry_days", 365)
	v.SetDefault("share.require_password", false)
	v.SetDefault("share.max_views", 0)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("logging.output", "stdout")

	v.SetDefault("rate_limit.enabled", true)
	v.SetDefault("rate_limit.requests_per_minute", 120)
	v.SetDefault("rate_limit.burst", 20)

	v.SetDefault("cors.allowed_origins", []string{"*"})
	v.SetDefault("cors.allowed_methods", []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"})
	v.SetDefault("cors.allowed_headers", []string{"Origin", "Content-Type", "Authorization", "X-Requested-With"})
	v.SetDefault("cors.allow_credentials", true)
	v.SetDefault("cors.max_age", "12h")
}

func (c *Config) resolvePaths(configPath string) {
	baseDir := "."
	if configPath != "" {
		baseDir = filepath.Dir(configPath)
	}

	if c.Database.SQLite.Path != "" && !filepath.IsAbs(c.Database.SQLite.Path) {
		c.Database.SQLite.Path = filepath.Join(baseDir, c.Database.SQLite.Path)
	}

	if c.Storage.Local.Root != "" && !filepath.IsAbs(c.Storage.Local.Root) {
		c.Storage.Local.Root = filepath.Join(baseDir, c.Storage.Local.Root)
	}

	if c.Storage.Chunked.TempDir != "" && !filepath.IsAbs(c.Storage.Chunked.TempDir) {
		c.Storage.Chunked.TempDir = filepath.Join(baseDir, c.Storage.Chunked.TempDir)
	}

}

func (c *Config) GetDSN() string {
	switch c.Database.Type {
	case "postgres":
		return fmt.Sprintf(
			"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			c.Database.Postgres.Host,
			c.Database.Postgres.Port,
			c.Database.Postgres.User,
			c.Database.Postgres.Password,
			c.Database.Postgres.DBName,
			c.Database.Postgres.SSLMode,
		)
	default:
		return c.Database.SQLite.Path
	}
}

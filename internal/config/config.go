package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Storage   StorageConfig   `mapstructure:"storage"`
	Logging   LoggingConfig   `mapstructure:"logging"`
	Pricing   PricingConfig   `mapstructure:"pricing"`
	Providers ProvidersConfig `mapstructure:"providers"`
	Metadata  MetadataConfig  `mapstructure:"metadata"`
	BodyStore BodyStoreConfig `mapstructure:"body_store"`
	Admin     AdminConfig     `mapstructure:"admin"`
}

// BodyStoreConfig configures optional request/response body archival to object
// storage. Backend is "none" (default), "s3", or "gcs". Credentials come from the
// cloud SDK's default chain (env AWS_*/GOOGLE_APPLICATION_CREDENTIALS or instance
// role) — the gateway stores no credentials itself.
type BodyStoreConfig struct {
	Backend    string `mapstructure:"backend"`    // none | s3 | gcs
	KeyPrefix  string `mapstructure:"key_prefix"` // optional object key prefix
	S3Bucket   string `mapstructure:"s3_bucket"`
	S3Region   string `mapstructure:"s3_region"`
	S3Endpoint string `mapstructure:"s3_endpoint"` // for S3-compatible stores (MinIO, R2)
	GCSBucket  string `mapstructure:"gcs_bucket"`
}

// MetadataConfig tunes the metadata-key discovery machinery: how often HyperLogLog
// cardinality state is flushed to the DB and how long the active-keys cache is held.
type MetadataConfig struct {
	HLLFlushInterval   time.Duration `mapstructure:"hll_flush_interval"`
	ActiveKeysCacheTTL time.Duration `mapstructure:"active_keys_cache_ttl"`
}

// AdminConfig holds the token that guards the admin (keys) and usage query HTTP
// API. When empty, those routes are disabled and the gateway serves proxy traffic
// only.
type AdminConfig struct {
	Token string `mapstructure:"token"`
}

type ServerConfig struct {
	Host                string        `mapstructure:"host"`
	Port                int           `mapstructure:"port"`
	ReadTimeout         time.Duration `mapstructure:"read_timeout"`
	WriteTimeout        time.Duration `mapstructure:"write_timeout"`
	UpstreamTimeout     time.Duration `mapstructure:"upstream_timeout"`
	StreamHeaderTimeout time.Duration `mapstructure:"stream_header_timeout"`
}

type StorageConfig struct {
	Driver   string         `mapstructure:"driver"`
	Postgres PostgresConfig `mapstructure:"postgres"`
}

type PostgresConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	Database string `mapstructure:"database"`
	SSLMode  string `mapstructure:"sslmode"`
	MaxConns int    `mapstructure:"max_conns"`
}

func (p *PostgresConfig) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		p.User, p.Password, p.Host, p.Port, p.Database, p.SSLMode)
}

type LoggingConfig struct {
	Level string `mapstructure:"level"`
}

type PricingConfig struct {
	RemoteURL            string        `mapstructure:"remote_url"`
	RefreshInterval      time.Duration `mapstructure:"refresh_interval"`
	FallbackFile         string        `mapstructure:"fallback_file"`
	AliasesFile          string        `mapstructure:"aliases_file"`
	DeprecatedModelsFile string        `mapstructure:"deprecated_models_file"`
}

type ProvidersConfig struct {
	OpenAI    ProviderConfig `mapstructure:"openai"`
	Anthropic ProviderConfig `mapstructure:"anthropic"`
	Gemini    ProviderConfig `mapstructure:"gemini"`
	Fireworks ProviderConfig `mapstructure:"fireworks"`
	Together  ProviderConfig `mapstructure:"together"`
	DeepSeek  ProviderConfig `mapstructure:"deepseek"`
}

type ProviderConfig struct {
	BaseURL string `mapstructure:"base_url"`
}

func Load() (*Config, error) {
	v := viper.New()
	setDefaults(v)
	bindEnv(v)

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func bindEnv(v *viper.Viper) {
	v.BindEnv("server.host", "HOST")
	v.BindEnv("server.port", "PORT")
	v.BindEnv("server.read_timeout", "READ_TIMEOUT")
	v.BindEnv("server.write_timeout", "WRITE_TIMEOUT")
	v.BindEnv("server.upstream_timeout", "UPSTREAM_TIMEOUT")
	v.BindEnv("server.stream_header_timeout", "STREAM_HEADER_TIMEOUT")

	v.BindEnv("storage.postgres.host", "POSTGRES_HOST")
	v.BindEnv("storage.postgres.port", "POSTGRES_PORT")
	v.BindEnv("storage.postgres.user", "POSTGRES_USER")
	v.BindEnv("storage.postgres.password", "POSTGRES_PASSWORD")
	v.BindEnv("storage.postgres.database", "POSTGRES_DB")
	v.BindEnv("storage.postgres.sslmode", "POSTGRES_SSLMODE")
	v.BindEnv("storage.postgres.max_conns", "POSTGRES_MAX_CONNS")

	v.BindEnv("logging.level", "LOG_LEVEL")

	v.BindEnv("body_store.backend", "BODY_STORAGE")
	v.BindEnv("body_store.key_prefix", "BODY_STORAGE_PREFIX")
	v.BindEnv("body_store.s3_bucket", "BODY_S3_BUCKET")
	v.BindEnv("body_store.s3_region", "BODY_S3_REGION")
	v.BindEnv("body_store.s3_endpoint", "BODY_S3_ENDPOINT")
	v.BindEnv("body_store.gcs_bucket", "BODY_GCS_BUCKET")

	v.BindEnv("pricing.remote_url", "PRICING_REMOTE_URL")
	v.BindEnv("pricing.refresh_interval", "PRICING_REFRESH_INTERVAL")
	v.BindEnv("pricing.fallback_file", "PRICING_FALLBACK_FILE")
	v.BindEnv("pricing.aliases_file", "PRICING_ALIASES_FILE")
	v.BindEnv("pricing.deprecated_models_file", "DEPRECATED_MODELS_FILE")

	v.BindEnv("providers.openai.base_url", "OPENAI_BASE_URL")
	v.BindEnv("providers.anthropic.base_url", "ANTHROPIC_BASE_URL")
	v.BindEnv("providers.gemini.base_url", "GEMINI_BASE_URL")
	v.BindEnv("providers.fireworks.base_url", "FIREWORKS_BASE_URL")
	v.BindEnv("providers.together.base_url", "TOGETHER_BASE_URL")
	v.BindEnv("providers.deepseek.base_url", "DEEPSEEK_BASE_URL")

	v.BindEnv("metadata.hll_flush_interval", "METADATA_HLL_FLUSH_INTERVAL")
	v.BindEnv("metadata.active_keys_cache_ttl", "METADATA_ACTIVE_KEYS_CACHE_TTL")

	v.BindEnv("admin.token", "ADMIN_TOKEN")
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 6560)
	v.SetDefault("server.read_timeout", 30*time.Second)
	v.SetDefault("server.write_timeout", 600*time.Second)
	v.SetDefault("server.upstream_timeout", 600*time.Second)
	v.SetDefault("server.stream_header_timeout", 0)

	v.SetDefault("storage.driver", "postgres")
	v.SetDefault("storage.postgres.host", "localhost")
	v.SetDefault("storage.postgres.port", 5432)
	v.SetDefault("storage.postgres.user", "")
	v.SetDefault("storage.postgres.password", "")
	v.SetDefault("storage.postgres.database", "majordomo_gateway")
	v.SetDefault("storage.postgres.sslmode", "disable")
	v.SetDefault("storage.postgres.max_conns", 20)

	v.SetDefault("logging.level", "info")

	v.SetDefault("body_store.backend", "none")
	v.SetDefault("body_store.key_prefix", "")

	v.SetDefault("pricing.remote_url", "https://www.llm-prices.com/current-v1.json")
	v.SetDefault("pricing.refresh_interval", time.Hour)
	v.SetDefault("pricing.fallback_file", "./pricing.json")
	v.SetDefault("pricing.aliases_file", "./model_aliases.json")
	v.SetDefault("pricing.deprecated_models_file", "./deprecated_models.json")

	v.SetDefault("providers.openai.base_url", "https://api.openai.com")
	v.SetDefault("providers.anthropic.base_url", "https://api.anthropic.com")
	v.SetDefault("providers.gemini.base_url", "https://generativelanguage.googleapis.com")
	v.SetDefault("providers.fireworks.base_url", "https://api.fireworks.ai/inference")
	v.SetDefault("providers.together.base_url", "https://api.together.xyz")
	v.SetDefault("providers.deepseek.base_url", "https://api.deepseek.com")

	v.SetDefault("metadata.hll_flush_interval", 60*time.Second)
	v.SetDefault("metadata.active_keys_cache_ttl", 5*time.Minute)

	v.SetDefault("admin.token", "")
}

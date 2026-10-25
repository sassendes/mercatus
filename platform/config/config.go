package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port                 string
	MetricsAddr          string
	DatabaseURL          string
	JWTSecret            string
	LogLevel             string
	LogFormat            string
	DBMaxOpenConns       int
	DBMaxIdleConns       int
	DBConnMaxLifetime    time.Duration
	SearchURL            string
	FraudURL             string
	FraudFailOpen        bool
	InternalToken        string
	CountryHeader        string
	CORSOrigins          []string
	TrustProxyHeaders    bool
	RateLimitRPS         float64
	RateLimitBurst       int
	ShutdownDelay        time.Duration
	ShutdownTimeout      time.Duration
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	IdleTimeout          time.Duration
	MaxBodyBytes         int64
	ReindexInterval      time.Duration
	SweepInterval        time.Duration
	S3Endpoint           string
	S3Region             string
	S3Bucket             string
	S3AccessKey          string
	S3SecretKey          string
	S3PublicURL          string
	S3PathStyle          bool
	Only                 []string
	RunBackgroundWorkers bool
}

func Load() (*Config, error) {
	c := &Config{
		Port:                 env("PORT", "8080"),
		MetricsAddr:          env("METRICS_ADDR", ":9090"),
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		JWTSecret:            os.Getenv("JWT_SECRET"),
		LogLevel:             env("LOG_LEVEL", "info"),
		LogFormat:            env("LOG_FORMAT", "json"),
		DBMaxOpenConns:       envInt("DB_MAX_OPEN_CONNS", 20),
		DBMaxIdleConns:       envInt("DB_MAX_IDLE_CONNS", 10),
		DBConnMaxLifetime:    envDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		SearchURL:            strings.TrimRight(os.Getenv("SEARCH_URL"), "/"),
		FraudURL:             strings.TrimRight(os.Getenv("FRAUD_URL"), "/"),
		FraudFailOpen:        envBool("FRAUD_FAIL_OPEN", false),
		InternalToken:        os.Getenv("INTERNAL_TOKEN"),
		CountryHeader:        strings.TrimSpace(os.Getenv("COUNTRY_HEADER")),
		CORSOrigins:          envList("CORS_ORIGINS"),
		TrustProxyHeaders:    envBool("TRUST_PROXY_HEADERS", false),
		RateLimitRPS:         envFloat("RATE_LIMIT_RPS", 20),
		RateLimitBurst:       envInt("RATE_LIMIT_BURST", 60),
		ShutdownDelay:        envDuration("SHUTDOWN_DELAY", 5*time.Second),
		ShutdownTimeout:      envDuration("SHUTDOWN_TIMEOUT", 20*time.Second),
		ReadTimeout:          envDuration("HTTP_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:         envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:          envDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
		MaxBodyBytes:         int64(envInt("HTTP_MAX_BODY_BYTES", 1<<20)),
		ReindexInterval:      envDuration("SEARCH_REINDEX_INTERVAL", 5*time.Minute),
		SweepInterval:        envDuration("SWEEP_INTERVAL", time.Minute),
		S3Endpoint:           strings.TrimRight(os.Getenv("S3_ENDPOINT"), "/"),
		S3Region:             env("S3_REGION", "us-east-1"),
		S3Bucket:             os.Getenv("S3_BUCKET"),
		S3AccessKey:          os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:          os.Getenv("S3_SECRET_KEY"),
		S3PublicURL:          strings.TrimRight(os.Getenv("S3_PUBLIC_URL"), "/"),
		S3PathStyle:          envBool("S3_PATH_STYLE", true),
		RunBackgroundWorkers: envBool("BACKGROUND_WORKERS", true),
	}
	return c, nil
}

func (c *Config) RequireDatabase() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	return nil
}

func (c *Config) RequireServe() error {
	if err := c.RequireDatabase(); err != nil {
		return err
	}
	if len(c.JWTSecret) < 32 {
		return errors.New("JWT_SECRET is required and must be at least 32 characters")
	}
	if (c.SearchURL != "" || c.FraudURL != "") && len(c.InternalToken) < 16 {
		return errors.New("INTERNAL_TOKEN of at least 16 characters is required when SEARCH_URL or FRAUD_URL is set")
	}
	if c.RateLimitRPS <= 0 || c.RateLimitBurst <= 0 {
		return fmt.Errorf("RATE_LIMIT_RPS and RATE_LIMIT_BURST must be positive")
	}
	return nil
}

func (c *Config) S3Enabled() bool {
	return c.S3Endpoint != "" && c.S3Bucket != "" && c.S3AccessKey != "" && c.S3SecretKey != ""
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64); err == nil {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v, err := time.ParseDuration(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return fallback
}

func envList(key string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(key), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

package app

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr, Origin, DataDir, APIKey, Model, BaseURL, ProviderName, DataRegion string
	Production                                                              bool
	Key                                                                     []byte
	Workers, QueueLimit, IPQuota, SessionQuota                              int
	PreparationIPQuota, PreparationSessionQuota                             int
	Retention, RequestTimeout                                               time.Duration
	BillingAdminKey                                                         string
}

func LoadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("invalid environment file line")
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" || strings.ContainsAny(key, " \t\r\n") {
			return errors.New("invalid environment variable name")
		}
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func LoadConfig() (Config, error) {
	c := Config{
		Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Origin: strings.TrimRight(env("PUBLIC_ORIGIN", "http://127.0.0.1:8080"), "/"),
		DataDir: env("DATA_DIR", "./data"), APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: os.Getenv("ANTHROPIC_MODEL"),
		BaseURL:      strings.TrimRight(env("ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "/"),
		ProviderName: env("PROVIDER_NAME", "Anthropic Claude"), DataRegion: env("DATA_REGION", "日本"),
		Production:      os.Getenv("APP_ENV") == "production",
		BillingAdminKey: strings.TrimSpace(os.Getenv("BILLING_ADMIN_KEY")),
	}
	var err error
	if c.BillingAdminKey != "" && (len(c.BillingAdminKey) < 32 || len(c.BillingAdminKey) > 128) {
		return c, errors.New("BILLING_ADMIN_KEY must contain 32 to 128 characters")
	}
	for _, setting := range []struct {
		name               string
		target             *int
		fallback, min, max int
	}{
		{"WORKER_COUNT", &c.Workers, 2, 1, 4}, {"QUEUE_LIMIT", &c.QueueLimit, 20, 1, 100},
		{"DIAGNOSES_PER_IP_HOUR", &c.IPQuota, 3, 1, 1000}, {"DIAGNOSES_PER_SESSION_DAY", &c.SessionQuota, 5, 1, 1000},
		{"PREPARATION_ACTIONS_PER_IP_HOUR", &c.PreparationIPQuota, 60, 1, 1000}, {"PREPARATION_ACTIONS_PER_SESSION_DAY", &c.PreparationSessionQuota, 120, 1, 2000},
	} {
		*setting.target, err = envInt(setting.name, setting.fallback, setting.min, setting.max)
		if err != nil {
			return c, err
		}
	}
	hours, err := envInt("RETENTION_HOURS", 168, 1, 168)
	if err != nil {
		return c, err
	}
	c.Retention = time.Duration(hours) * time.Hour
	seconds, err := envInt("REQUEST_TIMEOUT_SECONDS", 120, 10, 180)
	if err != nil {
		return c, err
	}
	c.RequestTimeout = time.Duration(seconds) * time.Second
	origin, err := url.Parse(c.Origin)
	if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return c, errors.New("PUBLIC_ORIGIN must be a complete origin without a path")
	}
	upstream, err := url.Parse(c.BaseURL)
	if err != nil || upstream.Host == "" || (upstream.Scheme != "https" && upstream.Scheme != "http") || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return c, errors.New("ANTHROPIC_BASE_URL is invalid")
	}
	if c.Production && (origin.Scheme != "https" || upstream.Scheme != "https") {
		return c, errors.New("production requires HTTPS for the public origin and upstream")
	}
	if len(c.ProviderName) > 100 || len(c.DataRegion) > 100 {
		return c, errors.New("provider or region label is too long")
	}
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return c, err
	}
	if err := os.Chmod(c.DataDir, 0700); err != nil {
		return c, err
	}
	keyText := os.Getenv("DATA_ENCRYPTION_KEY")
	if keyText != "" {
		c.Key, err = hex.DecodeString(keyText)
		if err != nil || len(c.Key) != 32 {
			return c, errors.New("DATA_ENCRYPTION_KEY must contain 64 hexadecimal characters")
		}
	} else {
		if c.Production {
			return c, errors.New("DATA_ENCRYPTION_KEY is required in production")
		}
		keyPath := filepath.Join(c.DataDir, "development.key")
		c.Key, err = os.ReadFile(keyPath)
		if errors.Is(err, os.ErrNotExist) {
			c.Key = make([]byte, 32)
			if _, err = rand.Read(c.Key); err != nil {
				return c, err
			}
			err = os.WriteFile(keyPath, c.Key, 0600)
		}
		if err != nil {
			return c, err
		}
		if len(c.Key) != 32 {
			return c, errors.New("development key is invalid; preserve the original key for the database")
		}
	}
	return c, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func envInt(name string, fallback, min, max int) (int, error) {
	value := fallback
	if raw := os.Getenv(name); raw != "" {
		var err error
		value, err = strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
	}
	if value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return value, nil
}

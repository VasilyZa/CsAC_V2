// Package config validates the service's environment before it opens listeners.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr                                           string
	DatabaseURL                                    string
	TLSCert, TLSKey                                string
	MediaRoot                                      string
	AccountKey, PasswordPepper, TrustedProxyToken  []byte
	ReleaseRoot, ReleaseGitHubRepo                 string
	ReleasePollInterval                            time.Duration
	ReleaseDeployWeb                               bool
	MaxFileBytes                                   int64
	FileRetention                                  time.Duration
	FileExtensions                                 []string
	MaxConnections                                 int
	PoolSize                                       int32
	RequireRegistrationEmail, RequireMemberEmail   bool
	SessionTTL, EventRetention                     time.Duration
	AdminUIDs                                      []int64
	AllowedOrigins                                 []string
	SMTPHost, SMTPUsername, SMTPPassword, SMTPFrom string
	SMTPPort                                       int
}

func Env() (Config, error) {
	c := Config{
		Addr: env("CSAC_ADDR", ":8443"), DatabaseURL: os.Getenv("CSAC_DATABASE_URL"),
		TLSCert: os.Getenv("CSAC_TLS_CERT"), TLSKey: os.Getenv("CSAC_TLS_KEY"),
		MediaRoot: env("CSAC_MEDIA_ROOT", "./data/media"), ReleaseRoot: os.Getenv("CSAC_RELEASE_ROOT"),
		ReleaseGitHubRepo: strings.TrimSpace(os.Getenv("CSAC_RELEASE_GITHUB_REPO")),
		SMTPHost:          os.Getenv("CSAC_SMTP_HOST"), SMTPFrom: os.Getenv("CSAC_SMTP_FROM"),
		SMTPUsername: os.Getenv("CSAC_SMTP_USERNAME"), SMTPPassword: os.Getenv("CSAC_SMTP_PASSWORD"),
	}
	if c.DatabaseURL == "" || c.TLSCert == "" || c.TLSKey == "" {
		return c, errors.New("CSAC_DATABASE_URL, CSAC_TLS_CERT and CSAC_TLS_KEY are required")
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return c, errors.New("CSAC_ADDR must include a port")
	}
	var err error
	if c.AccountKey, err = secret32("CSAC_ACCOUNT_KEY"); err != nil {
		return c, err
	}
	if c.PasswordPepper, err = secret32("CSAC_PASSWORD_PEPPER"); err != nil {
		return c, err
	}
	if len(c.PasswordPepper) == 0 {
		return c, errors.New("CSAC_PASSWORD_PEPPER or CSAC_PASSWORD_PEPPER_FILE is required")
	}
	if bytes.Equal(c.PasswordPepper, c.AccountKey) {
		return c, errors.New("CSAC_ACCOUNT_KEY and CSAC_PASSWORD_PEPPER must use different keys")
	}
	if c.TrustedProxyToken, err = secret32("CSAC_TRUSTED_PROXY_TOKEN"); err != nil {
		return c, err
	}
	if len(c.TrustedProxyToken) != 0 && (bytes.Equal(c.TrustedProxyToken, c.AccountKey) || bytes.Equal(c.TrustedProxyToken, c.PasswordPepper)) {
		return c, errors.New("CSAC_TRUSTED_PROXY_TOKEN requires a separate key")
	}
	if raw, ok := os.LookupEnv("CSAC_ADMIN_UIDS"); ok {
		c.AdminUIDs, err = adminUIDs(raw)
		if err != nil {
			return c, err
		}
	}
	if c.AllowedOrigins, err = origins(os.Getenv("CSAC_ALLOWED_ORIGINS")); err != nil {
		return c, err
	}
	if c.MaxFileBytes, err = integer("CSAC_MAX_FILE_BYTES", 64<<20, 1024, 1<<30); err != nil {
		return c, err
	}
	fileDays, err := integer("CSAC_FILE_RETENTION_DAYS", 3, 1, 3650)
	if err != nil {
		return c, err
	}
	c.FileRetention = time.Duration(fileDays) * 24 * time.Hour
	if c.FileExtensions, err = fileExtensions(os.Getenv("CSAC_FILE_ALLOWED_TYPES")); err != nil {
		return c, err
	}
	connections, err := integer("CSAC_MAX_CONNECTIONS", 4096, 16, 1000000)
	if err != nil {
		return c, err
	}
	c.MaxConnections = int(connections)
	pool, err := integer("CSAC_DB_POOL_SIZE", 32, 4, 256)
	if err != nil {
		return c, err
	}
	c.PoolSize = int32(pool)
	days, err := integer("CSAC_EVENT_RETENTION_DAYS", 7, 1, 90)
	if err != nil {
		return c, err
	}
	c.EventRetention = time.Duration(days) * 24 * time.Hour
	sessionDays, err := integer("CSAC_SESSION_DAYS", 30, 1, 90)
	if err != nil {
		return c, err
	}
	c.SessionTTL = time.Duration(sessionDays) * 24 * time.Hour
	pollMinutes, err := integer("CSAC_RELEASE_POLL_MINUTES", 360, 1, 1440)
	if err != nil {
		return c, err
	}
	c.ReleasePollInterval = time.Duration(pollMinutes) * time.Minute
	if c.ReleaseDeployWeb, err = boolean("CSAC_RELEASE_DEPLOY_WEB", false); err != nil {
		return c, err
	}
	if c.RequireRegistrationEmail, err = boolean("CSAC_REQUIRE_REGISTRATION_EMAIL", true); err != nil {
		return c, err
	}
	if c.RequireMemberEmail, err = boolean("CSAC_REQUIRE_MEMBER_EMAIL", true); err != nil {
		return c, err
	}
	port, err := integer("CSAC_SMTP_PORT", 465, 1, 65535)
	c.SMTPPort = int(port)
	if err != nil {
		return c, err
	}
	return c, nil
}

func adminUIDs(raw string) ([]int64, error) {
	ids := make([]int64, 0)
	if strings.TrimSpace(raw) == "" {
		return ids, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 1000 {
		return nil, errors.New("CSAC_ADMIN_UIDS supports up to 1000 entries")
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 || part[0] == '+' {
			return nil, errors.New("CSAC_ADMIN_UIDS expects comma-separated positive UIDs")
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

func origins(raw string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.Contains(u.Host, "*") {
			return nil, errors.New("CSAC_ALLOWED_ORIGINS expects exact HTTPS origins")
		}
		out = append(out, strings.ToLower(part))
	}
	return out, nil
}
func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && value != "" {
		return value
	}
	return fallback
}
func integer(name string, fallback, min, max int64) (int64, error) {
	value, err := strconv.ParseInt(env(name, strconv.FormatInt(fallback, 10)), 10, 64)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be in %d–%d", name, min, max)
	}
	return value, nil
}
func boolean(name string, fallback bool) (bool, error) {
	value, err := strconv.ParseBool(env(name, strconv.FormatBool(fallback)))
	if err != nil {
		return false, fmt.Errorf("%s requires true or false", name)
	}
	return value, nil
}

func fileExtensions(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	extensions := strings.Split(raw, ",")
	if len(extensions) > 256 {
		return nil, errors.New("CSAC_FILE_ALLOWED_TYPES supports up to 256 extensions")
	}
	for i, extension := range extensions {
		extension = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(extension)), ".")
		if len(extension) == 0 || len(extension) > 16 || strings.ContainsFunc(extension, func(ch rune) bool {
			return (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9')
		}) {
			return nil, errors.New("CSAC_FILE_ALLOWED_TYPES expects comma-separated file extensions")
		}
		extensions[i] = extension
	}
	slices.Sort(extensions)
	return slices.Compact(extensions), nil
}

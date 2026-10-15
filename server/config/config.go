// Package config reads the server's configuration from the environment only.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Config is every setting the server reads, with its environment variable.
type Config struct {
	// DatabaseURL is the Postgres connection string (HARNESS_DATABASE_URL). Required.
	DatabaseURL string
	// DirectoryFile is the JSON file of people, teams and tokens the server
	// authenticates and targets against (HARNESS_DIRECTORY_FILE). Required.
	DirectoryFile string
	// ListenAddr is the HTTP listen address (HARNESS_LISTEN_ADDR, default ":8080").
	ListenAddr string
	// BlobBackend selects the body store, "fs" or "s3" (HARNESS_BLOB_BACKEND, default "fs").
	BlobBackend string
	// BlobDir is the fs backend's root directory (HARNESS_BLOB_DIR, default "./data/blobs").
	BlobDir string
	// S3Endpoint is the host[:port] of an S3-compatible endpoint (HARNESS_S3_ENDPOINT).
	S3Endpoint string
	// S3Bucket is the bucket, created when absent (HARNESS_S3_BUCKET).
	S3Bucket string
	// S3Region is the bucket region (HARNESS_S3_REGION).
	S3Region string
	// S3AccessKey and S3SecretKey are static credentials; empty uses the
	// instance role (HARNESS_S3_ACCESS_KEY, HARNESS_S3_SECRET_KEY).
	S3AccessKey string
	S3SecretKey string
	// S3UseSSL selects https to the endpoint (HARNESS_S3_USE_SSL, default true).
	S3UseSSL bool
	// S3Prefix is prepended to every object key (HARNESS_S3_PREFIX).
	S3Prefix string
	// AllowHooks admits HOOK artifacts, which run code on every targeted
	// machine (HARNESS_ALLOW_HOOKS, default false).
	AllowHooks bool
}

func parseBool(name, v string, def bool) (bool, error) {
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return b, nil
}

// FromEnv builds the configuration from getenv, rejecting a missing
// required setting.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		DatabaseURL:   getenv("HARNESS_DATABASE_URL"),
		DirectoryFile: getenv("HARNESS_DIRECTORY_FILE"),
		ListenAddr:    getenv("HARNESS_LISTEN_ADDR"),
		BlobBackend:   strings.ToLower(getenv("HARNESS_BLOB_BACKEND")),
		BlobDir:       getenv("HARNESS_BLOB_DIR"),
		S3Endpoint:    getenv("HARNESS_S3_ENDPOINT"),
		S3Bucket:      getenv("HARNESS_S3_BUCKET"),
		S3Region:      getenv("HARNESS_S3_REGION"),
		S3AccessKey:   getenv("HARNESS_S3_ACCESS_KEY"),
		S3SecretKey:   getenv("HARNESS_S3_SECRET_KEY"),
		S3Prefix:      getenv("HARNESS_S3_PREFIX"),
	}
	var err error
	if c.S3UseSSL, err = parseBool("HARNESS_S3_USE_SSL", getenv("HARNESS_S3_USE_SSL"), true); err != nil {
		return Config{}, err
	}
	if c.AllowHooks, err = parseBool("HARNESS_ALLOW_HOOKS", getenv("HARNESS_ALLOW_HOOKS"), false); err != nil {
		return Config{}, err
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if c.BlobBackend == "" {
		c.BlobBackend = "fs"
	}
	if c.BlobDir == "" {
		c.BlobDir = "./data/blobs"
	}
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("HARNESS_DATABASE_URL is required"))
	}
	if c.DirectoryFile == "" {
		errs = append(errs, errors.New("HARNESS_DIRECTORY_FILE is required"))
	}
	switch c.BlobBackend {
	case "fs":
	case "s3":
		if c.S3Endpoint == "" || c.S3Bucket == "" {
			errs = append(errs, errors.New("HARNESS_S3_ENDPOINT and HARNESS_S3_BUCKET are required for the s3 blob backend"))
		}
	default:
		errs = append(errs, fmt.Errorf("HARNESS_BLOB_BACKEND must be fs or s3, got %q", c.BlobBackend))
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

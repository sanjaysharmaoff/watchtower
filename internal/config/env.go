// Package config turns environment variables into a plain struct.
//
// Nothing else in the codebase reads os.Getenv. Adapters receive the values they
// need through their factory, so they stay usable from tests and from other
// programs without an environment.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

const prefix = "WT_"

// LoadDotEnv reads a KEY=VALUE file and sets any variable not already present in
// the environment. Missing files are not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return sc.Err()
}

func lookup(key string) (string, bool) {
	if v, ok := os.LookupEnv(prefix + key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), true
	}
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), true
	}
	return "", false
}

func str(key, def string) string {
	if v, ok := lookup(key); ok {
		return v
	}
	return def
}

func integer(key string, def int) int {
	if v, ok := lookup(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func decimal(key string, def float64) float64 {
	if v, ok := lookup(key); ok {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

func boolean(key string, def bool) bool {
	if v, ok := lookup(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func duration(key string, def time.Duration) time.Duration {
	if v, ok := lookup(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

func list(key string, def []string) []string {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

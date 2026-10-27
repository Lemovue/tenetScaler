// Package envfile loads KEY=VALUE pairs from a .env file into the process
// environment without overriding variables that are already set.
package envfile

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Load reads path if it exists. A missing file is not an error.
func Load(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if line == 1 {
			text = strings.TrimPrefix(text, "\ufeff") // UTF-8 BOM (Windows PowerShell 5.1 writes one)
		}
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, val, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if !ok || key == "" {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	return sc.Err()
}

// Get returns the environment value for key, or def when unset or empty.
func Get(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

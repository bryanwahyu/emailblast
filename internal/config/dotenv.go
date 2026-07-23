// Package config loads a .env file into the process environment so secrets
// (SMTP password, sender identity, AWS creds) stay out of the command line and
// out of git. No external dependency.
package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotenv reads KEY=VALUE lines from path and sets them in the environment
// unless the key is already set (real env always wins over the file). Missing
// file is not an error — .env is optional. Lines starting with # and blanks are
// ignored; surrounding quotes on the value are stripped.
func LoadDotenv(path string) error {
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
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}

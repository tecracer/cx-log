package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// loadDotEnv reads KEY=VALUE lines from path into the process environment.
// Variables already set in the environment win, so `ACXD_REGION=x cx-log`
// still overrides the file. A missing file is not an error unless required
// is true (an explicit --env-file).
//
// Supported syntax: blank lines, "# comments", optional "export " prefix,
// and values wrapped in single or double quotes.
func loadDotEnv(path string, required bool) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !required {
			return nil
		}
		return fmt.Errorf("env file: %w", err)
	}
	defer f.Close()

	if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "cx-log: warning: %s is readable by other users (mode %04o); run: chmod 600 %s\n",
			path, fi.Mode().Perm(), path)
	}

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("env file %s:%d: expected KEY=VALUE", path, n)
		}
		val = unquote(strings.TrimSpace(val))
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	// Strip trailing inline comment on unquoted values: KEY=value # note
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// envFileFromArgs finds --env-file/-env-file in args before flag parsing,
// because the .env file must be loaded before flag defaults are read from
// the environment.
func envFileFromArgs(args []string) (path string, explicit bool) {
	for i, a := range args {
		if a == "--" {
			break
		}
		for _, p := range []string{"--env-file", "-env-file"} {
			if a == p && i+1 < len(args) {
				return args[i+1], true
			}
			if v, ok := strings.CutPrefix(a, p+"="); ok {
				return v, true
			}
		}
	}
	return ".env", false
}

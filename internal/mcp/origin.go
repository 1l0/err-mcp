package mcp

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// OriginFileName stores the browser origin that successfully paired with the
// app bridge. It sits next to the token file in the per-user config directory.
const OriginFileName = "mcp-origin"

// DefaultOriginPath returns os.UserConfigDir()/err/mcp-origin.
func DefaultOriginPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(dir, "err", OriginFileName), nil
}

// ReadPairedOrigin reads the persisted app origin. It tightens the file
// permissions to 0600 when needed.
func ReadPairedOrigin(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read paired origin: %w", err)
	}
	origin, err := normalizeOrigin(string(data))
	if err != nil {
		return "", fmt.Errorf("paired origin file %s: %w", path, err)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("restrict paired origin permissions: %w", err)
		}
	}
	return origin, nil
}

// WritePairedOrigin persists the first app origin that paired with the bridge.
func WritePairedOrigin(path, origin string) error {
	origin, err := normalizeOrigin(origin)
	if err != nil {
		return err
	}
	return writePrivateFile(path, []byte(origin+"\n"))
}

// RemovePairedOrigin deletes the pairing file. A missing file is not an error.
func RemovePairedOrigin(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove paired origin: %w", err)
	}
	return nil
}

// isLocalhostOrigin reports whether origin is http://localhost with an
// optional port. Local development servers use this origin, and it is
// loopback-only by definition.
func isLocalhostOrigin(origin string) bool {
	normalized, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return false
	}
	return u.Scheme == "http" && strings.EqualFold(u.Hostname(), "localhost")
}

// normalizeOrigin accepts a serialized browser origin such as
// https://app.example or http://127.0.0.1:8080.
func normalizeOrigin(s string) (string, error) {
	origin := strings.TrimSpace(s)
	if origin == "" {
		return "", errors.New("origin is empty")
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("invalid origin %q: %w", origin, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("origin must use http or https: %q", origin)
	}
	if u.Host == "" {
		return "", fmt.Errorf("origin has no host: %q", origin)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("origin must not contain a path, query, fragment or credentials: %q", origin)
	}
	return u.Scheme + "://" + u.Host, nil
}

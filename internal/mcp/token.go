package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenFileName is the file under the per-user config directory that stores the
// generated bridge token.
const TokenFileName = "mcp-token"

// DefaultTokenPath returns os.UserConfigDir()/err/mcp-token, for example
// ~/Library/Application Support/err/mcp-token on macOS or
// $XDG_CONFIG_HOME/err/mcp-token on Linux.
func DefaultTokenPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(dir, "err", TokenFileName), nil
}

// LoadOrCreateToken returns the token stored at path. When the file does not
// exist it generates a new token, writes it with 0600 permissions (in a 0700
// directory), and reports created=true. An existing token is reused so a server
// restart does not invalidate the app's saved configuration.
func LoadOrCreateToken(path string) (token string, created bool, err error) {
	token, err = LoadToken(path)
	if err == nil {
		return token, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	token, err = RotateToken(path)
	if err != nil {
		return "", false, err
	}
	return token, true, nil
}

// LoadToken reads an existing token file. It never creates or rewrites the
// token; it only tightens the file permissions to 0600 when needed.
func LoadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token, err := normalizeToken(string(data))
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("restrict token file permissions: %w", err)
		}
	}
	return token, nil
}

// RotateToken generates a new token and atomically replaces the file at path.
// The caller is responsible for printing the new value once and for making a
// running server pick it up (restart).
func RotateToken(path string) (string, error) {
	token, err := GenerateToken()
	if err != nil {
		return "", err
	}
	if err := WriteToken(path, token); err != nil {
		return "", err
	}
	return token, nil
}

// WriteToken stores token at path with 0600 permissions. The parent directory
// is created with 0700 permissions. The write is atomic: a temporary file is
// renamed over the destination, so a crash cannot leave a half-written token.
func WriteToken(path, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to write an empty token")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return errors.New("token must not contain whitespace")
	}
	return writePrivateFile(path, []byte(token+"\n"))
}

// writePrivateFile atomically writes data to path with 0600 permissions in a
// 0700 directory. It is shared by the token and pairing-origin files.
func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode unchanged; keep the config
	// directory private even if it was created by an older version.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restrict config directory permissions: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".mcp-config-*")
	if err != nil {
		return fmt.Errorf("create config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set config temp permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config file: %w", err)
	}
	return nil
}

// GenerateToken returns a random 256-bit token as lowercase hex.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// TokenFingerprint returns a stable, non-reversible label for a token. It lets
// a later start identify the stored token without printing the secret.
func TokenFingerprint(token string) string {
	if token == "" {
		return "(none)"
	}
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:6])
}

func normalizeToken(s string) (string, error) {
	token := strings.TrimSpace(s)
	if token == "" {
		return "", errors.New("token is empty")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("token must not contain whitespace")
	}
	return token, nil
}

//go:build !js

package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateTokenPersistsAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "err", TokenFileName)

	first, created, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	if !created {
		t.Fatal("first call did not report a created token")
	}
	if len(first) != 64 {
		t.Fatalf("token length = %d, want 64 hex characters", len(first))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("token file: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token permissions = %o, want 0600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat token directory: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("directory permissions = %o, want 0700", perm)
	}

	second, created, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("second LoadOrCreateToken: %v", err)
	}
	if created {
		t.Fatal("second call reported a created token")
	}
	if second != first {
		t.Fatalf("token changed across restarts: %q -> %q", first, second)
	}
}

func TestRotateTokenReplacesTheSavedValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokenFileName)

	first, err := RotateToken(path)
	if err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	second, err := RotateToken(path)
	if err != nil {
		t.Fatalf("second RotateToken: %v", err)
	}
	if first == second {
		t.Fatal("rotation returned the same token")
	}

	loaded, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if loaded != second {
		t.Fatalf("stored token = %q, want the rotated value %q", loaded, second)
	}
}

func TestLoadTokenTightensPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokenFileName)
	if err := os.WriteFile(path, []byte("secret-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got != "secret-token" {
		t.Fatalf("token = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 0600", perm)
	}
}

func TestLoadTokenRejectsEmptyOrMultilineFiles(t *testing.T) {
	for name, content := range map[string]string{
		"empty":     "",
		"blank":     " \n\t",
		"multiline": "first\nsecond\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), TokenFileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadToken(path); err == nil {
				t.Fatal("LoadToken accepted an invalid token file")
			}
		})
	}
}

func TestLoadTokenMissingFileIsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokenFileName)
	_, err := LoadToken(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadToken error = %v, want a not-exist error", err)
	}
}

func TestWriteTokenRefusesEmptyOrWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokenFileName)
	for _, value := range []string{"", "   ", "two words", "line\nbreak"} {
		if err := WriteToken(path, value); err == nil {
			t.Fatalf("WriteToken(%q) succeeded", value)
		}
	}
}

func TestWriteTokenCreatesMissingParentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "err")
	path := filepath.Join(dir, TokenFileName)
	if err := WriteToken(path, "abc123"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("directory permissions = %o, want 0700", perm)
	}
}

func TestTokenFingerprintIsStableAndNotTheToken(t *testing.T) {
	token := strings.Repeat("a", 64)
	fp := TokenFingerprint(token)
	if fp == TokenFingerprint(strings.Repeat("b", 64)) {
		t.Fatal("different tokens have the same fingerprint")
	}
	if strings.Contains(fp, token) || strings.Contains(fp, token[:8]) {
		t.Fatalf("fingerprint leaks the token: %q", fp)
	}
	if TokenFingerprint(token) != fp {
		t.Fatal("fingerprint is not stable")
	}
	if TokenFingerprint("") != "(none)" {
		t.Fatalf("empty fingerprint = %q", TokenFingerprint(""))
	}
}

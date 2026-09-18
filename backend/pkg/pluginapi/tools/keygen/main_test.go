package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateKeysNoOverwrite(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "publisher")
	if err := generate(prefix); err != nil {
		t.Fatal(err)
	}
	public, _ := os.ReadFile(prefix + ".public")
	private, _ := os.ReadFile(prefix + ".private")
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(public)))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(private)))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte("manifest"), ed25519.Sign(priv, []byte("manifest"))) {
		t.Fatal("key pair mismatch")
	}
	if err := generate(prefix); err == nil {
		t.Fatal("must not overwrite publisher keys")
	}
	after, _ := os.ReadFile(prefix + ".private")
	if string(after) != string(private) {
		t.Fatal("private key changed")
	}
}

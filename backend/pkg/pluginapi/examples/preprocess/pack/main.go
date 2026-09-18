// pack assembles a built example binary and its UI into a .s2plugin file.
package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	binary := flag.String("binary", "", "Compiled plugin binary")
	output := flag.String("out", "", "New .s2plugin output path")
	source := flag.String("source", "pkg/pluginapi/examples/preprocess", "Example source directory")
	target := flag.String("target", runtime.GOOS+"-"+runtime.GOARCH, "Target GOOS-GOARCH")
	version := flag.String("version", "", "Version embedded in the compiled binary")
	accountType := flag.String("account-type", "oauth", "Account type embedded in the binary: oauth or apikey")
	signingKey := flag.String("signing-key", "", "File containing Base64 Ed25519 private key")
	keyID := flag.String("key-id", "", "Trusted publisher key ID")
	flag.Parse()
	if *accountType != "oauth" && *accountType != "apikey" {
		return errors.New("-account-type must be oauth or apikey")
	}
	if *binary == "" || *output == "" {
		return errors.New("-binary and -out are required")
	}
	if (*signingKey == "") != (*keyID == "") {
		return errors.New("-signing-key and -key-id must be provided together")
	}
	data, err := os.ReadFile(*binary)
	if err != nil {
		return err
	}
	runtimePath := "bin/preprocess"
	if strings.HasPrefix(*target, "windows-") {
		runtimePath += ".exe"
	}
	files := map[string][]byte{runtimePath: data}
	for _, name := range []string{"ui/index.html", "ui/app.js"} {
		files[name], err = os.ReadFile(filepath.Join(*source, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
	}
	raw, err := os.ReadFile(filepath.Join(*source, "manifest.source.json"))
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	if *version != "" {
		manifest["version"] = *version
	}
	capabilities, ok := manifest["capabilities"].([]any)
	if !ok || len(capabilities) != 1 {
		return errors.New("example manifest must have one capability")
	}
	capability, ok := capabilities[0].(map[string]any)
	if !ok {
		return errors.New("invalid example capability")
	}
	capability["account_type"] = *accountType
	manifest["runtimes"] = map[string]any{*target: map[string]string{"path": runtimePath}}
	hashes := map[string]string{}
	for name, data := range files {
		digest := sha256.Sum256(data)
		hashes[name] = hex.EncodeToString(digest[:])
	}
	manifest["files"] = hashes
	raw, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	files["manifest.json"] = raw
	if *signingKey != "" {
		keyText, err := os.ReadFile(*signingKey)
		if err != nil {
			return err
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyText)))
		if err != nil || len(key) != ed25519.PrivateKeySize {
			return errors.New("signing key must be a Base64 Ed25519 private key")
		}
		private := ed25519.PrivateKey(key)
		public, validPublic := private.Public().(ed25519.PublicKey)
		if !validPublic {
			return errors.New("invalid Ed25519 public key")
		}
		signature := map[string]string{"algorithm": "ed25519", "key_id": *keyID, "public_key": base64.StdEncoding.EncodeToString(public), "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(private, raw))}
		files["signature.json"], err = json.Marshal(signature)
		if err != nil {
			return err
		}
	}
	file, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	ok = false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(*output)
		}
	}()
	writer := zip.NewWriter(file)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			return err
		}
		if _, err = entry.Write(files[name]); err != nil {
			return err
		}
	}
	if err = writer.Close(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	ok = true
	fmt.Println("Created", *output)
	return nil
}

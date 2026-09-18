// keygen generates an Ed25519 publisher key pair without printing the private key.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	output := flag.String("out", "", "New key path prefix; creates .private and .public files")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(1)
	}
	if err := generate(*output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Created public key:", *output+".public")
	fmt.Println("Keep the .private file outside version control and release packages.")
}
func generate(prefix string) error {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(prefix), 0700); err != nil {
		return err
	}
	writeNew := func(path string, data []byte) error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.WriteString(base64.StdEncoding.EncodeToString(data) + "\n")
		closeErr := f.Close()
		if err != nil {
			_ = os.Remove(path)
			return err
		}
		if closeErr != nil {
			_ = os.Remove(path)
		}
		return closeErr
	}
	if err = writeNew(prefix+".private", private); err != nil {
		return err
	}
	if err = writeNew(prefix+".public", public); err != nil {
		_ = os.Remove(prefix + ".private")
		return err
	}
	return nil
}

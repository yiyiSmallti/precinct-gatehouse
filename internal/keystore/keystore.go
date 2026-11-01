// Package keystore resolves provider API keys. In personal mode keys live in
// the OS keychain (Windows Credential Manager, macOS Keychain, Secret Service
// on Linux); env: and file: references exist for headless machines and tests.
package keystore

import (
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service name used for all Gatehouse entries.
const Service = "precinct-gatehouse"

// Resolve returns the secret a key_ref points to. An empty ref means "no key".
func Resolve(ref string) (string, error) {
	kind, name, _ := strings.Cut(ref, ":")
	switch kind {
	case "":
		return "", nil
	case "env":
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(name)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case "keychain":
		v, err := keyring.Get(Service, name)
		if err != nil {
			return "", fmt.Errorf("keychain entry %q: %w (store it with: gatehouse key set %s)", name, err, name)
		}
		return v, nil
	}
	return "", fmt.Errorf("unknown key_ref %q", ref)
}

// Set stores a secret in the OS keychain.
func Set(name, secret string) error { return keyring.Set(Service, name, secret) }

// Delete removes a secret from the OS keychain.
func Delete(name string) error { return keyring.Delete(Service, name) }

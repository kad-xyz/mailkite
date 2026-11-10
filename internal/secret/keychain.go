// Package secret reads and writes mail passwords in the macOS Keychain.
// Passwords never appear in argv, env, files or logs.
package secret

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	securityBin = "/usr/bin/security"
	service     = "mailkite"
)

// ErrNotFound means the Keychain has no item for the account.
var ErrNotFound = errors.New("secret: not found in keychain")

// Store gives access to per-email passwords.
type Store interface {
	Get(email string) (string, error)
	// Set stores a password interactively: security itself prompts on the TTY.
	Set(email string) error
}

// Keychain implements Store with /usr/bin/security. Run, if set, replaces
// process execution (tests); otherwise Set attaches the child to the terminal.
type Keychain struct {
	Run func(name string, args ...string) ([]byte, error)
}

func (k Keychain) Get(email string) (string, error) {
	args := []string{"find-generic-password", "-s", service, "-a", email, "-w"}
	var out []byte
	var err error
	if k.Run != nil {
		out, err = k.Run(securityBin, args...)
	} else {
		out, err = exec.Command(securityBin, args...).Output()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("keychain get: %w", err)
	}
	pw := strings.TrimSuffix(string(out), "\n") // security -w appends one newline
	return strings.TrimSuffix(pw, "\r"), nil
}

func (k Keychain) Set(email string) error {
	// -w must be last and valueless so security prompts for the password on the TTY.
	args := []string{"add-generic-password", "-U", "-s", service, "-a", email, "-w"}
	if k.Run != nil {
		_, err := k.Run(securityBin, args...)
		return err
	}
	cmd := exec.Command(securityBin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain set: %w", err)
	}
	return nil
}

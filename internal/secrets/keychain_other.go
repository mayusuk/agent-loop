//go:build !darwin

package secrets

import "errors"

var errKeychainUnsupported = errors.New("the keychain backend is available on macOS only; set secrets.backend = \"envfile\"")

func keychainGet(path, service, account string) (string, bool, error) {
	return "", false, errKeychainUnsupported
}

func keychainSet(path, service, account, label, value, app string) error {
	return errKeychainUnsupported
}

func keychainDelete(path, service, account string) error {
	return errKeychainUnsupported
}

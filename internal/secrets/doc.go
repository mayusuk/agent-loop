// Package secrets implements the SecretStore port, with macOS Keychain and
// env/file backends, and issues short-lived per-run tokens for each identity
// (TECHNICAL-SPEC T12, §7.3).
package secrets

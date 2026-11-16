package contract

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is the contract version this core speaks (INTEGRATION-SPEC §2).
const Version = "1.0"

// Major and Minor are the parts of Version.
const (
	Major = 1
	Minor = 0
)

// ErrUnsupportedVersion is returned for a contract version this core can't
// support: another major, or a minor above the core's (INTEGRATION-SPEC §2).
var ErrUnsupportedVersion = errors.New("unsupported contract version")

// ParseVersion parses a MAJOR.MINOR contract version.
func ParseVersion(v string) (major, minor int, err error) {
	ma, mi, ok := strings.Cut(v, ".")
	if !ok {
		return 0, 0, fmt.Errorf("contract version %q is not MAJOR.MINOR", v)
	}
	major, err1 := parseVersionPart(ma)
	minor, err2 := parseVersionPart(mi)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("contract version %q is not MAJOR.MINOR", v)
	}
	return major, minor, nil
}

// parseVersionPart accepts a decimal number with no sign and no leading zero.
func parseVersionPart(s string) (int, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, errors.New("bad version part")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("bad version part")
		}
	}
	return strconv.Atoi(s)
}

// CheckVersion reports whether the core supports a result written for
// contract version v: the same major, and a minor no higher than the core's
// (INTEGRATION-SPEC §2, work plan D2).
func CheckVersion(v string) error {
	major, minor, err := ParseVersion(v)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupportedVersion, err)
	}
	if major != Major || minor > Minor {
		return fmt.Errorf("%w: %s (core speaks %s)", ErrUnsupportedVersion, v, Version)
	}
	return nil
}

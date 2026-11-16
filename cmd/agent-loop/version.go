package main

import "runtime/debug"

// version is set at link time with -ldflags "-X main.version=<v>".
var version = "dev"

// readBuildInfo is replaced in tests.
var readBuildInfo = debug.ReadBuildInfo

// resolvedVersion returns the link-time version if one was set, else the
// module version of a `go install`ed build, else "dev".
func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := readBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

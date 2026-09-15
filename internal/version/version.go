// Package version reports the build version. Version, Commit and Date are set
// with -ldflags "-X" by the release build; a `go install` build falls back to
// the module version recorded in the binary.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at link time.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// String returns the version, "dev" when unknown.
func String() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Library returns the goRDFlib module version linked into the binary.
func Library() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/tggo/goRDFlib" {
				if d.Replace != nil {
					return d.Replace.Version + " (replaced)"
				}
				return d.Version
			}
		}
	}
	return "unknown"
}

// Long returns a multi-line description for the version subcommand.
func Long() string {
	commit, date := Commit, Date
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && commit == "":
				commit = s.Value
			case s.Key == "vcs.time" && date == "":
				date = s.Value
			}
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	if date == "" {
		date = "unknown"
	}
	return fmt.Sprintf("sparql-server %s\ncommit:   %s\nbuilt:    %s\ngoRDFlib: %s\ngo:       %s %s/%s\n",
		String(), commit, date, Library(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

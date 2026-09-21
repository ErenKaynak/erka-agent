package buildinfo

import "runtime/debug"

// These values are replaced with -ldflags for release builds.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info describes the source and build identity of an erka executable.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Current returns the build identity compiled into the running executable.
// Development builds without -ldflags fall back to the Go toolchain's
// embedded version-control stamp, so bug reports always name a commit.
func Current() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date}
	if info.Commit != "unknown" && info.Date != "unknown" {
		return info
	}
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	return fromSettings(info, build.Settings)
}

func fromSettings(info Info, settings []debug.BuildSetting) Info {
	modified := false
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Commit == "unknown" && setting.Value != "" {
				info.Commit = setting.Value
				if len(info.Commit) > 12 {
					info.Commit = info.Commit[:12]
				}
			}
		case "vcs.time":
			if info.Date == "unknown" && setting.Value != "" {
				info.Date = setting.Value
			}
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if modified && info.Commit != "unknown" {
		info.Commit += "-dirty"
	}
	return info
}

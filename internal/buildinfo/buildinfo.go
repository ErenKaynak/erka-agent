package buildinfo

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
func Current() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
	}
}

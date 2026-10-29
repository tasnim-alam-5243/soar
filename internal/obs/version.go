package obs

// Set at build time with -ldflags "-X .../internal/obs.Version=...".
var (
	Version = "dev"
	Commit  = "none"
)

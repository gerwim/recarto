// Package version holds the build version of Recarto.
package version

// Version is the current Recarto version. It is a var (not a const) so release
// builds can stamp it via the linker: -ldflags "-X .../internal/version.Version=...".
var Version = "0.1.0-dev"

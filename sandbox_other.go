//go:build !darwin && !linux

package main

// valen runs every shell command inside an OS sandbox (sandbox.go), which only
// exists for macOS (sandbox-exec) and Linux (bubblewrap). Rather than silently
// run commands unsandboxed, building for any other OS fails here: the compiler
// error names the undefined identifier below, which spells out the reason.
var _ = unsupported_platform_valen_shell_sandbox_supports_only_macOS_and_Linux__see_sandbox_go

// Command aty runs the user's shell inside a PTY and turns a leading "?" at a
// fresh prompt into a model-generated command.
package main

import "os"

// GoReleaser replaces version with the release version at build time.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

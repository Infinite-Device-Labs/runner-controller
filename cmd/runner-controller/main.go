// Command runner-controller starts ephemeral GitHub Actions runners on
// demand, from runner scale sets, as systemd units within one memory budget.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "runner-controller: not implemented yet")
	os.Exit(1)
}

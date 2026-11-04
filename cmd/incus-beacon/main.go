package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tallica/incus-beacon/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}
	fmt.Fprintln(os.Stderr, "incus-beacon: nothing here yet")
}

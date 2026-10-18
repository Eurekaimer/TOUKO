package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Eurekaimer/TOUKO/internal/buildinfo"
	"github.com/Eurekaimer/TOUKO/internal/desktop"
)

func main() {
	stateDir := flag.String("state-dir", "", "Local workspace directory (same selection rules as touko)")
	port := flag.Int("port", 0, "Loopback HTTP port (0 selects an available port)")
	showVersion := flag.Bool("version", false, "Show version and source commit without opening a window")
	flag.Parse()
	if flag.NArg() != 0 || *port < 0 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "usage: touko-desktop [--version] [--state-dir PATH] [--port 0..65535]")
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println("touko-desktop", buildinfo.String())
		return
	}
	if err := desktop.Run(*stateDir, *port, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

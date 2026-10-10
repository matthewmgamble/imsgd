// Command imsgd is a headless iMessage server for the local network.
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `imsgd - iMessage LAN server

Usage:
  imsgd <command> [flags]

Commands:
  run              Start the server in the foreground
  install-agent    Install and start imsgd as a launchd agent (starts at login)
  uninstall-agent  Stop and remove the launchd agent
  status           Show agent and server status
  doctor           Check permissions and environment
  keys             Manage API keys: list | create NAME | revoke NAME
  gen-cert         Create a self-signed TLS certificate for the LAN
  version          Print version
  debug            Developer tools

Run "imsgd <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "run":
		err = runServe(args)
	case "doctor":
		err = runDoctor(args)
	case "install-agent":
		err = runInstallAgent(args)
	case "uninstall-agent":
		err = runUninstallAgent(args)
	case "status":
		err = runStatus(args)
	case "keys":
		err = runKeys(args)
	case "gen-cert":
		err = runGenCert(args)
	case "debug":
		err = runDebug(args)
	case "version", "-v", "--version":
		fmt.Println("imsgd", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

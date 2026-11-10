// Command mailkite is an MCP server that reads Apple Mail's local data.
package main

import (
	"fmt"
	"os"
)

const version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("mailkite " + version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: mailkite version")
	os.Exit(2)
}

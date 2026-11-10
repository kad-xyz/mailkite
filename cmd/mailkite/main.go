// Command mailkite is an MCP server that reads Apple Mail's local data.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"

	"github.com/kad-xyz/mailkite/internal/config"
	"github.com/kad-xyz/mailkite/internal/fts"
	"github.com/kad-xyz/mailkite/internal/imapx"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/secret"
	"github.com/kad-xyz/mailkite/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev" // set at release build time via -ldflags "-X main.version=..."

// tuneRuntime trims the Go runtime for a server that answers one stdio request at a time:
// two Ps instead of one per core (fewer per-P caches, GC workers and threads), and GC at
// 50% heap growth, so the heap goal is 2 MB instead of 4 MB over a ~1 MB live heap.
// Both only change work done while a request runs; GOMAXPROCS and GOGC still override.
func tuneRuntime() {
	if os.Getenv("GOMAXPROCS") == "" && runtime.GOMAXPROCS(0) > 2 {
		runtime.GOMAXPROCS(2)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
}

func main() {
	tuneRuntime()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mailkite:", err)
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	openStore := func() (*maildata.Store, error) { return maildata.Open(maildata.Options{}) }
	openIndex := func() (*fts.Index, error) { return fts.Open("") }
	switch {
	case len(args) == 0:
		srv, closeAll := server.NewLazy(openStore, openIndex, server.Options{})
		defer closeAll()
		return srv.Run(ctx, &mcp.StdioTransport{})
	case args[0] == "version":
		fmt.Println("mailkite " + version)
		return nil
	case args[0] == "setup":
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		cfgPath, err := config.Path()
		if err != nil {
			return err
		}
		return runSetup(ctx, os.Stdin, os.Stdout, st.Accounts, secret.Keychain{}, imapx.Verify, cfgPath)
	case args[0] == "restore":
		if len(args) != 2 {
			return fmt.Errorf("usage: mailkite restore <file.eml|archive day directory>")
		}
		cfgPath, err := config.Path()
		if err != nil {
			return err
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		return runRestore(ctx, realRestoreDeps(cfg, os.Stdout), args[1])
	case args[0] == "index":
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		ix, err := openIndex()
		if err != nil {
			return err
		}
		defer ix.Close()
		if err := ix.Rebuild(ctx, st); err != nil {
			return err
		}
		n, err := ix.Count(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("indexed %d docs\n", n)
		return nil
	}
	fmt.Fprintln(os.Stderr, "usage: mailkite [version|index|setup|restore <path>]")
	os.Exit(2)
	return nil
}

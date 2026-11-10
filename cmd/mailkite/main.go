// Command mailkite is an MCP server that reads Apple Mail's local data.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/kad-xyz/mailkite/internal/fts"
	"github.com/kad-xyz/mailkite/internal/maildata"
	"github.com/kad-xyz/mailkite/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "dev"

func main() {
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
	fmt.Fprintln(os.Stderr, "usage: mailkite [version|index]")
	os.Exit(2)
	return nil
}

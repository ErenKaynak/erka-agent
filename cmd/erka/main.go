package main

import (
	"context"
	"os"

	"github.com/ErenKaynak/erka-agent/internal/buildinfo"
	"github.com/ErenKaynak/erka-agent/internal/cli"
)

func main() {
	ctx, stop := signalContext(context.Background())
	defer stop()
	code := cli.Execute(
		ctx,
		os.Args[1:],
		os.Stdout,
		os.Stderr,
		buildinfo.Current(),
	)
	os.Exit(code)
}

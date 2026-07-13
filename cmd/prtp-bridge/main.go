package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"prtp-bridge/internal/prtpbridge/config"
	"prtp-bridge/internal/prtpbridge/gateway"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var configPath string
	var instance string
	fs.StringVar(&configPath, "config", "", "path to prtp-bridge JSON config")
	fs.StringVar(&instance, "instance", "", "optional instance key from config.instances")
	_ = fs.Parse(args)

	root, err := config.Load(configPath)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := root.ResolveInstance(instance)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := gateway.Serve(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Println(`usage: prtp-bridge serve -config /etc/kroma/prtp-bridge.json [--instance NET0]

subcommands:
  serve   run the config-driven UDP/WebSocket/audio bridge`)
}

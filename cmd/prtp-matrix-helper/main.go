package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"prtp-bridge/internal/prtpbridge/config"
	"prtp-bridge/internal/prtpbridge/matrixhelper"
)

func main() {
	var configPath string
	var instance string
	flag.StringVar(&configPath, "config", "", "path to prtp-bridge JSON config")
	flag.StringVar(&instance, "instance", "", "optional instance key from config.instances")
	flag.Parse()

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
	server := matrixhelper.NewServer(matrixhelper.Options{
		SocketPath: cfg.MatrixHelperSocket,
		MatrixAddr: cfg.MatrixAddr,
		MatrixPort: cfg.MatrixPort,
		Debug:      cfg.Debug.Matrix,
	})
	log.Printf("matrix helper listening on unix:%s matrix_addr=%s matrix_port=%s", cfg.MatrixHelperSocket, cfg.MatrixAddr, cfg.MatrixPort)
	if err := server.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}

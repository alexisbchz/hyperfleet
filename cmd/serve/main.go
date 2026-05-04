package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alexis-bouchez/hyperfleet/internal/api"
	"github.com/alexis-bouchez/hyperfleet/internal/auth"
	"github.com/alexis-bouchez/hyperfleet/internal/sshd"
	"github.com/alexis-bouchez/hyperfleet/internal/vmmgr"
	"github.com/containerd/containerd/v2/client"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/urfave/cli/v3"
)

func main() {
	app := &cli.Command{
		Name:  "serve",
		Usage: "hyperfleet daemon (REST API + SSH gateway)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "addr", Sources: cli.EnvVars("ADDR"), Value: ":8080", Usage: "HTTP listen address"},
			&cli.StringFlag{Name: "ssh-addr", Sources: cli.EnvVars("SSH_ADDR"), Value: ":2222", Usage: "SSH gateway listen address"},
			&cli.StringFlag{Name: "api-key", Sources: cli.EnvVars("HYPERFLEET_API_KEY"), Usage: "API key (generated ephemerally if unset)"},
			&cli.StringFlag{Name: "containerd-sock", Sources: cli.EnvVars("CONTAINERD_SOCK"), Value: "/run/containerd/containerd.sock"},
			&cli.StringFlag{Name: "namespace", Sources: cli.EnvVars("CONTAINERD_NAMESPACE"), Value: "hyperfleet"},
			&cli.StringFlag{Name: "snapshotter", Sources: cli.EnvVars("SNAPSHOTTER"), Value: "devmapper"},
			&cli.StringFlag{Name: "firecracker-bin", Sources: cli.EnvVars("FIRECRACKER_BIN"), Value: "./bin/firecracker"},
			&cli.StringFlag{Name: "kernel-path", Sources: cli.EnvVars("KERNEL_PATH"), Value: "./assets/vmlinux"},
			&cli.StringFlag{Name: "work-root", Sources: cli.EnvVars("WORK_ROOT"), Value: "./run"},
		},
		Action: run,
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	addr := cmd.String("addr")
	sshAddr := cmd.String("ssh-addr")
	containerdSock := cmd.String("containerd-sock")
	namespace := cmd.String("namespace")
	snapshotter := cmd.String("snapshotter")
	firecrackerBin := cmd.String("firecracker-bin")
	kernelPath := cmd.String("kernel-path")
	workRoot := cmd.String("work-root")

	apiKey := cmd.String("api-key")
	if apiKey == "" {
		apiKey = randomKey()
		log.Printf("HYPERFLEET_API_KEY not set; generated ephemeral key: %s", apiKey)
	}

	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return fmt.Errorf("create work root: %w", err)
	}

	ctrd, err := client.New(containerdSock)
	if err != nil {
		return fmt.Errorf("connect containerd at %s: %w", containerdSock, err)
	}
	defer ctrd.Close()

	mgr := vmmgr.New(vmmgr.Config{
		Containerd:     ctrd,
		Namespace:      namespace,
		Snapshotter:    snapshotter,
		FirecrackerBin: firecrackerBin,
		KernelPath:     kernelPath,
		WorkRoot:       workRoot,
	})
	if err := mgr.Load(ctx); err != nil {
		return fmt.Errorf("load machine state: %w", err)
	}

	router := chi.NewMux()
	router.Use(auth.HTTPMiddleware(apiKey))
	humaAPI := humachi.New(router, huma.DefaultConfig("hyperfleet", "0.1.0"))
	api.Register(humaAPI, mgr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	sshSrv, err := sshd.New(sshd.Config{
		Addr:        sshAddr,
		APIKey:      apiKey,
		HostKeyPath: filepath.Join(workRoot, "sshd_host_ed25519"),
		Manager:     mgr,
	})
	if err != nil {
		return fmt.Errorf("init sshd: %w", err)
	}

	httpErr := make(chan error, 1)
	go func() {
		log.Printf("http listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
			return
		}
		httpErr <- nil
	}()

	sshErrCh := make(chan error, 1)
	go func() {
		log.Printf("ssh listening on %s (user=<machine-id>, password=HYPERFLEET_API_KEY)", sshAddr)
		sshErrCh <- sshSrv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
		log.Println("context cancelled, shutting down")
	case <-sigCh:
		log.Println("signal received, shutting down")
	case err := <-httpErr:
		if err != nil {
			log.Printf("http server error: %v", err)
		}
	case err := <-sshErrCh:
		if err != nil {
			log.Printf("ssh server error: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	if err := sshSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("ssh shutdown: %v", err)
	}
	mgr.Shutdown(shutdownCtx)
	log.Println("shutdown complete")
	return nil
}

func randomKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/alexis-bouchez/hyperfleet/internal/auth"
	"github.com/alexis-bouchez/hyperfleet/internal/vmmgr"
	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Addr        string // ":2222"
	APIKey      string
	HostKeyPath string // persistent ed25519 private key
	Manager     *vmmgr.Manager
}

type Server struct {
	srv *gssh.Server
}

func New(cfg Config) (*Server, error) {
	hostKey, err := loadOrGenerateHostKey(cfg.HostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}

	srv := &gssh.Server{
		Addr: cfg.Addr,
		PasswordHandler: func(_ gssh.Context, password string) bool {
			return auth.Check(password, cfg.APIKey)
		},
		Handler: handler(cfg.Manager),
	}
	srv.AddHostKey(hostKey)
	return &Server{srv: srv}, nil
}

// ListenAndServe starts the SSH listener; blocks until Shutdown or error.
func (s *Server) ListenAndServe() error {
	if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, gssh.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func handler(mgr *vmmgr.Manager) gssh.Handler {
	return func(s gssh.Session) {
		machineID := s.User()
		if machineID == "" {
			io.WriteString(s, "no machine id provided as username\r\n")
			s.Exit(2)
			return
		}

		conn, err := mgr.Attach(machineID)
		if err != nil {
			fmt.Fprintf(s, "attach %s: %v\r\n", machineID, err)
			s.Exit(1)
			return
		}
		defer conn.Close()

		fmt.Fprintf(s, "[hyperfleet] attached to %s; press Enter for a prompt\r\n", machineID)

		// Pipe SSH session <-> VM serial.
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(conn, s); done <- struct{}{} }()
		go func() { _, _ = io.Copy(s, conn); done <- struct{}{} }()
		<-done
	}
}

func loadOrGenerateHostKey(path string) (ssh.Signer, error) {
	if data, err := os.ReadFile(path); err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		return signer, nil
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal pkcs8: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	log.Printf("generated SSH host key at %s", path)

	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parse generated key: %w", err)
	}
	return signer, nil
}

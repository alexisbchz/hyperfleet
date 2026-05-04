package vmmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/lucsky/cuid"
	"github.com/opencontainers/image-spec/identity"
)

var ErrNotFound = errors.New("machine not found")

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusExited  Status = "exited"
	StatusFailed  Status = "failed"
)

type Machine struct {
	ID        string     `json:"id"`
	Image     string     `json:"image"`
	Status    Status     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ExitedAt  *time.Time `json:"exitedAt,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type Config struct {
	Containerd     *client.Client
	Namespace      string
	Snapshotter    string
	FirecrackerBin string
	KernelPath     string
	WorkRoot       string
}

type Manager struct {
	cfg Config

	mu       sync.RWMutex
	machines map[string]*entry
}

type entry struct {
	m       Machine
	cancel  context.CancelFunc
	done    chan struct{}
	console *Console
}

func New(cfg Config) *Manager {
	return &Manager{
		cfg:      cfg,
		machines: make(map[string]*entry),
	}
}

func (mg *Manager) Create(ctx context.Context, image string) (Machine, error) {
	id := cuid.New()
	now := time.Now().UTC()

	bgCtx, cancel := context.WithCancel(context.Background())
	bgCtx = namespaces.WithNamespace(bgCtx, mg.cfg.Namespace)

	e := &entry{
		m: Machine{
			ID:        id,
			Image:     image,
			Status:    StatusPending,
			CreatedAt: now,
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}

	mg.mu.Lock()
	mg.machines[id] = e
	snap := e.m
	mg.mu.Unlock()

	go mg.run(bgCtx, e)

	return snap, nil
}

func (mg *Manager) List(ctx context.Context) []Machine {
	mg.mu.RLock()
	defer mg.mu.RUnlock()
	out := make([]Machine, 0, len(mg.machines))
	for _, e := range mg.machines {
		out = append(out, e.m)
	}
	return out
}

func (mg *Manager) Get(ctx context.Context, id string) (Machine, error) {
	mg.mu.RLock()
	defer mg.mu.RUnlock()
	e, ok := mg.machines[id]
	if !ok {
		return Machine{}, ErrNotFound
	}
	return e.m, nil
}

func (mg *Manager) Delete(ctx context.Context, id string) error {
	mg.mu.Lock()
	e, ok := mg.machines[id]
	mg.mu.Unlock()
	if !ok {
		return ErrNotFound
	}

	e.cancel()
	select {
	case <-e.done:
	case <-ctx.Done():
		return ctx.Err()
	}

	mg.mu.Lock()
	delete(mg.machines, id)
	mg.mu.Unlock()
	return nil
}

// Attach returns a ReadWriteCloser bound to the machine's serial console.
// Returns ErrNotFound if the machine doesn't exist, or an error if the VM
// is not yet running (no console attached) or has already exited.
func (mg *Manager) Attach(id string) (io.ReadWriteCloser, error) {
	mg.mu.RLock()
	e, ok := mg.machines[id]
	mg.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}

	mg.mu.RLock()
	console := e.console
	status := e.m.Status
	mg.mu.RUnlock()

	if console == nil {
		return nil, fmt.Errorf("machine %s is %s", id, status)
	}
	return console.Attach()
}

func (mg *Manager) Shutdown(ctx context.Context) {
	mg.mu.RLock()
	entries := make([]*entry, 0, len(mg.machines))
	for _, e := range mg.machines {
		entries = append(entries, e)
	}
	mg.mu.RUnlock()

	for _, e := range entries {
		e.cancel()
	}
	for _, e := range entries {
		select {
		case <-e.done:
		case <-ctx.Done():
			return
		}
	}
}

func (mg *Manager) run(ctx context.Context, e *entry) {
	defer close(e.done)

	leaseCtx, doneLease, err := mg.cfg.Containerd.WithLease(ctx, leases.WithExpiration(15*time.Minute))
	if err != nil {
		mg.markFailed(e, fmt.Errorf("acquire lease: %w", err))
		return
	}
	defer doneLease(context.Background())

	log.Printf("[%s] pulling %s", e.m.ID, e.m.Image)
	image, err := mg.cfg.Containerd.Pull(leaseCtx, e.m.Image,
		client.WithPullSnapshotter(mg.cfg.Snapshotter),
		client.WithPullUnpack,
	)
	if err != nil {
		mg.markFailed(e, fmt.Errorf("pull: %w", err))
		return
	}

	rootfsDevice, releaseSnap, err := mg.prepareSnapshot(leaseCtx, image, e.m.ID)
	if err != nil {
		mg.markFailed(e, fmt.Errorf("snapshot: %w", err))
		return
	}
	defer releaseSnap()

	workDir := filepath.Join(mg.cfg.WorkRoot, e.m.ID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		mg.markFailed(e, fmt.Errorf("workdir: %w", err))
		return
	}
	defer os.RemoveAll(workDir)

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		mg.markFailed(e, fmt.Errorf("stdin pipe: %w", err))
		return
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		mg.markFailed(e, fmt.Errorf("stdout pipe: %w", err))
		return
	}

	fcm, err := mg.boot(leaseCtx, rootfsDevice, workDir, stdinR, stdoutW)
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		mg.markFailed(e, fmt.Errorf("boot: %w", err))
		return
	}

	// Close child-side fds in the parent so EOF propagates correctly.
	stdinR.Close()
	stdoutW.Close()

	console := newConsole(stdinW, stdoutR)
	mg.mu.Lock()
	e.console = console
	mg.mu.Unlock()

	mg.markRunning(e)
	log.Printf("[%s] running (rootfs=%s)", e.m.ID, rootfsDevice)

	waitErr := fcm.Wait(leaseCtx)
	console.close()
	_ = fcm.StopVMM()

	if waitErr != nil && leaseCtx.Err() == nil {
		mg.markFailed(e, fmt.Errorf("wait: %w", waitErr))
		return
	}
	mg.markExited(e)
	log.Printf("[%s] exited", e.m.ID)
}

func (mg *Manager) prepareSnapshot(ctx context.Context, image client.Image, id string) (string, func(), error) {
	diffIDs, err := image.RootFS(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("image rootfs: %w", err)
	}
	parent := identity.ChainID(diffIDs).String()

	snapID := "hyperfleet-" + id
	snapSvc := mg.cfg.Containerd.SnapshotService(mg.cfg.Snapshotter)

	mounts, err := snapSvc.Prepare(ctx, snapID, parent)
	if err != nil {
		return "", nil, fmt.Errorf("snapshot prepare: %w", err)
	}
	if len(mounts) == 0 {
		return "", nil, fmt.Errorf("snapshotter returned no mounts")
	}

	cleanup := func() {
		_ = snapSvc.Remove(context.Background(), snapID)
	}
	return mounts[0].Source, cleanup, nil
}

func (mg *Manager) boot(ctx context.Context, rootfsDevice, workDir string, stdin io.Reader, stdout io.Writer) (*firecracker.Machine, error) {
	socketPath := filepath.Join(workDir, "firecracker.sock")
	_ = os.Remove(socketPath)

	cfg := firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: mg.cfg.KernelPath,
		KernelArgs:      "console=ttyS0 reboot=k panic=1 pci=off init=/bin/sh",
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(rootfsDevice),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(1),
			MemSizeMib: firecracker.Int64(256),
			Smt:        firecracker.Bool(false),
		},
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(mg.cfg.FirecrackerBin).
		WithSocketPath(socketPath).
		WithStdin(stdin).
		WithStdout(stdout).
		WithStderr(io.Discard).
		Build(ctx)

	m, err := firecracker.NewMachine(ctx, cfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		return nil, fmt.Errorf("new machine: %w", err)
	}
	if err := m.Start(ctx); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	return m, nil
}

func (mg *Manager) markRunning(e *entry) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	now := time.Now().UTC()
	e.m.Status = StatusRunning
	e.m.StartedAt = &now
}

func (mg *Manager) markExited(e *entry) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	now := time.Now().UTC()
	e.m.Status = StatusExited
	e.m.ExitedAt = &now
}

func (mg *Manager) markFailed(e *entry, err error) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	now := time.Now().UTC()
	e.m.Status = StatusFailed
	e.m.ExitedAt = &now
	e.m.Error = err.Error()
	log.Printf("[%s] failed: %v", e.m.ID, err)
}


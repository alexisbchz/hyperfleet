package vmmgr

import (
	"context"
	"encoding/json"
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
	// LeaseExpiration is the lifetime of each containerd lease. The manager
	// renews the lease in the background at half this interval so VMs that
	// outlive a single lease window keep their image content protected from GC.
	// Zero disables renewal and falls back to a 24h one-shot lease.
	LeaseExpiration time.Duration
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
	mg.saveStateLocked(e.m)
	snap := e.m
	mg.mu.Unlock()

	go mg.run(bgCtx, e)

	return snap, nil
}

// Load reads persisted machine state from disk and rehydrates the in-memory
// index. Machines that were running or pending at the time of the previous
// shutdown are marked failed with "lost on restart" and their work directories
// are cleaned up; we have no way to reattach to the firecracker process across
// a server restart in v0.
func (mg *Manager) Load(ctx context.Context) error {
	dir := filepath.Join(mg.cfg.WorkRoot, "state")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state dir: %w", err)
	}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			log.Printf("state read %s: %v", ent.Name(), err)
			continue
		}
		var m Machine
		if err := json.Unmarshal(data, &m); err != nil {
			log.Printf("state unmarshal %s: %v", ent.Name(), err)
			continue
		}
		if m.Status == StatusRunning || m.Status == StatusPending {
			now := time.Now().UTC()
			m.Status = StatusFailed
			m.ExitedAt = &now
			if m.Error == "" {
				m.Error = "lost on restart"
			}
			_ = os.RemoveAll(filepath.Join(mg.cfg.WorkRoot, m.ID))
		}
		closed := make(chan struct{})
		close(closed)
		e := &entry{
			m:      m,
			cancel: func() {},
			done:   closed,
		}
		mg.mu.Lock()
		mg.machines[m.ID] = e
		mg.saveStateLocked(e.m)
		mg.mu.Unlock()
	}
	return nil
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
	mg.removeState(id)
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

	expiry := mg.cfg.LeaseExpiration
	if expiry <= 0 {
		expiry = 24 * time.Hour
	}

	ls := mg.cfg.Containerd.LeasesService()
	lease, err := ls.Create(ctx, leases.WithRandomID(), leases.WithExpiration(expiry))
	if err != nil {
		mg.markFailed(e, fmt.Errorf("acquire lease: %w", err))
		return
	}
	leaseRef := &leaseHolder{cur: lease}
	defer func() {
		_ = ls.Delete(context.Background(), leaseRef.get())
	}()
	leaseCtx := leases.WithLease(ctx, lease.ID)

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

	snapResource := leases.Resource{
		Type: "snapshots/" + mg.cfg.Snapshotter,
		ID:   "hyperfleet-" + e.m.ID,
	}
	if err := ls.AddResource(leaseCtx, lease, snapResource); err != nil {
		log.Printf("[%s] lease add resource: %v", e.m.ID, err)
	}

	renewerDone := make(chan struct{})
	go mg.renewLease(ctx, e.m.ID, ls, leaseRef, snapResource, expiry, renewerDone)
	defer func() { <-renewerDone }()

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

// leaseHolder wraps the active lease so the renewal goroutine can swap it out
// while run()'s deferred cleanup still deletes whichever one is current.
type leaseHolder struct {
	mu  sync.Mutex
	cur leases.Lease
}

func (h *leaseHolder) get() leases.Lease {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}

func (h *leaseHolder) swap(l leases.Lease) leases.Lease {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.cur
	h.cur = l
	return prev
}

func (mg *Manager) renewLease(ctx context.Context, id string, ls leases.Manager, holder *leaseHolder, res leases.Resource, expiry time.Duration, done chan<- struct{}) {
	defer close(done)
	interval := expiry / 2
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fresh, err := ls.Create(ctx, leases.WithRandomID(), leases.WithExpiration(expiry))
			if err != nil {
				log.Printf("[%s] lease renew create: %v", id, err)
				continue
			}
			if err := ls.AddResource(ctx, fresh, res); err != nil {
				log.Printf("[%s] lease renew add resource: %v", id, err)
				_ = ls.Delete(ctx, fresh)
				continue
			}
			old := holder.swap(fresh)
			if err := ls.Delete(ctx, old); err != nil {
				log.Printf("[%s] lease renew delete old: %v", id, err)
			}
		}
	}
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
		KernelArgs:      "console=ttyS0 reboot=k panic=1 acpi=off init=/bin/sh",
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
	mg.saveStateLocked(e.m)
}

func (mg *Manager) markExited(e *entry) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	now := time.Now().UTC()
	e.m.Status = StatusExited
	e.m.ExitedAt = &now
	mg.saveStateLocked(e.m)
}

func (mg *Manager) markFailed(e *entry, err error) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	now := time.Now().UTC()
	e.m.Status = StatusFailed
	e.m.ExitedAt = &now
	e.m.Error = err.Error()
	mg.saveStateLocked(e.m)
	log.Printf("[%s] failed: %v", e.m.ID, err)
}

// saveStateLocked persists a machine's metadata to disk. Caller must hold mg.mu.
// Failures are logged but not propagated: state-on-disk is a best-effort index
// for rehydration after a restart, and a write failure shouldn't abort the
// VM lifecycle.
func (mg *Manager) saveStateLocked(m Machine) {
	if mg.cfg.WorkRoot == "" {
		return
	}
	dir := filepath.Join(mg.cfg.WorkRoot, "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[%s] state mkdir: %v", m.ID, err)
		return
	}
	final := filepath.Join(dir, m.ID+".json")
	tmp := final + ".tmp"
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		log.Printf("[%s] state marshal: %v", m.ID, err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("[%s] state write: %v", m.ID, err)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		log.Printf("[%s] state rename: %v", m.ID, err)
	}
}

func (mg *Manager) removeState(id string) {
	if mg.cfg.WorkRoot == "" {
		return
	}
	_ = os.Remove(filepath.Join(mg.cfg.WorkRoot, "state", id+".json"))
}


package vmmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/alexis-bouchez/hyperfleet/internal/initd"
	"github.com/alexis-bouchez/hyperfleet/internal/network"
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
	// Network, if non-nil, gives each VM a tap on the shared bridge with a
	// static IP, default route, and DNS configured via the kernel ip= boot
	// param plus a /etc/resolv.conf written into the rootfs before boot.
	// When nil, VMs boot with loopback only.
	Network *network.Manager
	// InitdPath is the host-side path to the static hyperfleet-init binary
	// that gets injected into the rootfs at /sbin/hyperfleet-init before
	// boot. Empty falls back to "./bin/hyperfleet-init".
	InitdPath string
}

type Manager struct {
	cfg Config

	mu       sync.RWMutex
	machines map[string]*entry
	// nextCID is a monotonic counter of guest vsock CIDs. CIDs 0–2 are
	// reserved (hypervisor / local / host); we start at 3. Each VM gets a
	// fresh CID; we never reuse, so an ID collision after restart can't
	// confuse the kernel's vsock state.
	nextCID uint32
}

type entry struct {
	m       Machine
	cancel  context.CancelFunc
	done    chan struct{}
	console *Console
	// vsockUDS is the host-side unix socket path firecracker created for
	// the VM's vsock device. The initd client dials it and writes
	// "CONNECT <port>\n" to reach the in-guest HTTP server.
	vsockUDS string
}

func New(cfg Config) *Manager {
	return &Manager{
		cfg:      cfg,
		machines: make(map[string]*entry),
		nextCID:  3,
	}
}

func (mg *Manager) allocCID() uint32 {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	c := mg.nextCID
	mg.nextCID++
	return c
}

// initdPath resolves the configured initd binary, falling back to the
// repo-relative default.
func (mg *Manager) initdPath() string {
	if mg.cfg.InitdPath != "" {
		return mg.cfg.InitdPath
	}
	return "./bin/hyperfleet-init"
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

// InitdClient returns an *initd.Client targeting the in-guest control plane
// of a running VM. Returns ErrNotFound if the machine doesn't exist, or an
// error if the VM hasn't reached the running state yet (no vsock UDS).
func (mg *Manager) InitdClient(id string) (*initd.Client, error) {
	mg.mu.RLock()
	e, ok := mg.machines[id]
	mg.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	mg.mu.RLock()
	uds := e.vsockUDS
	status := e.m.Status
	mg.mu.RUnlock()
	if uds == "" {
		return nil, fmt.Errorf("machine %s is %s", id, status)
	}
	return initd.New(uds, initd.VsockPort), nil
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

	var alloc network.Allocation
	if mg.cfg.Network != nil {
		a, err := mg.cfg.Network.Allocate(e.m.ID)
		if err != nil {
			mg.markFailed(e, fmt.Errorf("network: %w", err))
			return
		}
		alloc = a
		defer mg.cfg.Network.Release(e.m.ID, alloc)
	}

	// Inject the static initd binary (and resolv.conf, when networking is
	// enabled) into the rootfs in a single mount→write→unmount pass. This
	// must complete before firecracker opens the device.
	if err := prepareRootfs(rootfsDevice, mg.initdPath(), alloc.DNS); err != nil {
		mg.markFailed(e, fmt.Errorf("prepare rootfs: %w", err))
		return
	}

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

	cid := mg.allocCID()
	vsockUDS := filepath.Join(workDir, "vsock.sock")
	fcm, err := mg.boot(leaseCtx, rootfsDevice, workDir, stdinR, stdoutW, alloc, cid, vsockUDS)
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
	e.vsockUDS = vsockUDS
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

// prepareRootfs mounts the rootfs block device read-write, copies the static
// hyperfleet-init binary into /sbin/hyperfleet-init, optionally writes
// /etc/resolv.conf with the allocated DNS server, and unmounts. Done once
// before firecracker opens the device. The Firecracker SDK can pass
// nameservers via /proc/net/pnp, but that requires the guest to symlink
// resolv.conf to it; we use Alpine images out of the box so we just write
// the file.
func prepareRootfs(device, initdSrc, dns string) error {
	mp, err := os.MkdirTemp("", "hf-rootfs-")
	if err != nil {
		return fmt.Errorf("mkdir mountpoint: %w", err)
	}
	defer os.Remove(mp)

	if out, err := exec.Command("mount", device, mp).CombinedOutput(); err != nil {
		return fmt.Errorf("mount: %w (%s)", err, string(out))
	}
	defer func() {
		if out, err := exec.Command("umount", mp).CombinedOutput(); err != nil {
			log.Printf("umount %s: %v (%s)", mp, err, string(out))
		}
	}()

	if initdSrc != "" {
		sbin := filepath.Join(mp, "sbin")
		if err := os.MkdirAll(sbin, 0o755); err != nil {
			return fmt.Errorf("mkdir sbin: %w", err)
		}
		if err := copyFile(initdSrc, filepath.Join(sbin, "hyperfleet-init"), 0o755); err != nil {
			return fmt.Errorf("install initd: %w", err)
		}
	}

	if dns != "" {
		etc := filepath.Join(mp, "etc")
		if err := os.MkdirAll(etc, 0o755); err != nil {
			return fmt.Errorf("mkdir etc: %w", err)
		}
		if err := os.WriteFile(filepath.Join(etc, "resolv.conf"), []byte("nameserver "+dns+"\n"), 0o644); err != nil {
			return fmt.Errorf("write resolv.conf: %w", err)
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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

func (mg *Manager) boot(ctx context.Context, rootfsDevice, workDir string, stdin io.Reader, stdout io.Writer, alloc network.Allocation, cid uint32, vsockUDS string) (*firecracker.Machine, error) {
	socketPath := filepath.Join(workDir, "firecracker.sock")
	_ = os.Remove(socketPath)
	_ = os.Remove(vsockUDS)

	cfg := firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: mg.cfg.KernelPath,
		KernelArgs:      "console=ttyS0 reboot=k panic=1 acpi=off init=/sbin/hyperfleet-init",
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
		VsockDevices: []firecracker.VsockDevice{{
			ID:   "vsock0",
			Path: vsockUDS,
			CID:  cid,
		}},
	}

	if alloc.TapName != "" {
		cfg.NetworkInterfaces = firecracker.NetworkInterfaces{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				HostDevName: alloc.TapName,
				MacAddress:  alloc.MacAddress,
				IPConfiguration: &firecracker.IPConfiguration{
					IPAddr:      net.IPNet{IP: alloc.IP, Mask: alloc.Mask},
					Gateway:     alloc.Gateway,
					Nameservers: []string{alloc.DNS},
					IfName:      "eth0",
				},
			},
		}}
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


// Command hyperfleet-forgejo-plugin is a Forgejo Runner v2 (go-plugin) backend
// that runs each CI job inside a hyperfleet microVM.
//
// It is launched as a subprocess by the runner (handshake from
// code.forgejo.org/forgejo/runner/v12/act/plugin) and serves the
// pluginv1.BackendPlugin gRPC API. Every RPC translates to one or more
// HTTP calls against a hyperfleet daemon's REST API:
//
//	Create  → POST   /machines                        (and poll until running)
//	Start   → GET    /machines/{id}/healthz           (wait for the in-guest initd)
//	Exec    → POST   /machines/{id}/exec              (frame stream → stdout/stderr/exit)
//	CopyIn  → PUT    /machines/{id}/files?path=...    (tar archive)
//	CopyLocal → host-side tar of src, then PUT files
//	CopyOut → GET    /machines/{id}/files?path=...
//	UpdateEnv → GET  /files?path=... + parse KEY=VALUE
//	IsHealthy → GET  /machines/{id}/healthz
//	Remove   → DELETE /machines/{id}
//
// Configuration is taken from the runner's plugin options map (forwarded as
// CreateRequest.BackendOptions on every Create call) plus environment:
//
//	HYPERFLEET_API_URL   (default: http://localhost:8080)
//	HYPERFLEET_API_KEY   (required)
//
// The plugin holds no per-VM state of its own: hyperfleet's daemon owns the
// machine map, and we use the machine ID as the environment ID we hand back
// to the runner.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	pluginsdk "code.forgejo.org/forgejo/runner/v12/act/plugin"
	pluginv1 "code.forgejo.org/forgejo/runner/v12/act/plugin/proto/v1"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	envAPIURL = "HYPERFLEET_API_URL"
	envAPIKey = "HYPERFLEET_API_KEY"

	defaultAPIURL = "http://localhost:8080"

	// Frame constants mirror internal/initd/protocol.go in the hyperfleet
	// repo. We don't import that package because it would force a Go module
	// dependency between the plugin and the daemon.
	frameStdout = 1
	frameStderr = 2
	frameExit   = 3
	frameError  = 4

	frameHeaderSize = 5

	// pollInterval is how often Create polls GET /machines/{id} while
	// waiting for the daemon to flip status from "pending" to "running".
	pollInterval = 250 * time.Millisecond
	// readyTimeout caps the wait for image pull + boot + initd up.
	readyTimeout = 5 * time.Minute
)

func main() {
	apiURL := os.Getenv(envAPIURL)
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	apiKey := os.Getenv(envAPIKey)
	// We don't fail fast on a missing key: the runner may be smoke-testing
	// the binary without intending to launch jobs. The first real RPC will
	// surface the auth failure through hyperfleet's 401.

	srv := &server{
		apiURL: strings.TrimRight(apiURL, "/"),
		apiKey: apiKey,
		http: &http.Client{
			Timeout: 0, // streaming endpoints (exec, tar) need no overall cap
		},
	}

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: pluginsdk.Handshake,
		Plugins: map[string]goplugin.Plugin{
			pluginsdk.PluginName: &pluginsdk.BackendGRPCPlugin{Impl: srv},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}

type server struct {
	pluginv1.UnimplementedBackendPluginServer

	apiURL string
	apiKey string
	http   *http.Client
}

// ---- HTTP helpers ----

func (s *server) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.apiURL+path, body)
	if err != nil {
		return nil, err
	}
	if s.apiKey != "" {
		req.Header.Set("X-API-Key", s.apiKey)
	}
	return req, nil
}

func (s *server) do(req *http.Request) (*http.Response, error) {
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("hyperfleet %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// ---- BackendPlugin RPCs ----

func (s *server) Capabilities(_ context.Context, _ *pluginv1.CapabilitiesRequest) (*pluginv1.CapabilitiesResponse, error) {
	return &pluginv1.CapabilitiesResponse{
		Name:                       "hyperfleet",
		RootPath:                   "/shared",
		ActPath:                    "/shared/act",
		ToolCachePath:              "/shared/toolcache",
		PathVariableName:           "PATH",
		DefaultPathVariable:        "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		PathSeparator:              ":",
		SupportsDockerActions:      false,
		ManagesOwnNetworking:       true,
		SupportsServiceContainers:  false,
		EnvironmentCaseInsensitive: false,
		SupportsLocalCopy:          true,
		RunnerContext: map[string]string{
			"os":         "Linux",
			"arch":       "x86_64",
			"temp":       "/tmp",
			"tool_cache": "/shared/toolcache",
		},
	}, nil
}

type createBody struct {
	Image string `json:"image"`
}

// schemePrefix is what the runner prepends to the configured arg when the
// label scheme is "hyperfleet" (no built-in handling in act/runner/labels.go
// for our custom scheme — it falls through to the default case that emits
// "<scheme>://<arg>"). Strip it back out here so we hand a clean OCI image
// reference to the daemon.
const schemePrefix = "hyperfleet://"

func (s *server) resolveImage(req *pluginv1.CreateRequest) string {
	// Order of preference:
	//  1. CreateRequest.Image: set when the workflow declares `container:`
	//     directly. We treat the value as an OCI ref and pass through,
	//     stripping the "hyperfleet://" scheme if the runner happened to
	//     include it.
	//  2. BackendOptions["label_arg"]: populated from the part of the
	//     runs-on label after the scheme (e.g. "//docker.io/library/alpine:3.20"
	//     for `runs-on: hyperfleet:hyperfleet://docker.io/library/alpine:3.20`).
	//     This is the path the runner takes when the image is encoded in the
	//     label rather than a `container:` block.
	if image := strings.TrimPrefix(req.GetImage(), schemePrefix); image != "" {
		return image
	}
	if arg, ok := req.GetBackendOptions()["label_arg"]; ok {
		return strings.TrimPrefix(arg, "//")
	}
	return ""
}

func (s *server) Create(ctx context.Context, req *pluginv1.CreateRequest) (*pluginv1.CreateResponse, error) {
	image := s.resolveImage(req)
	if image == "" {
		return nil, status.Errorf(codes.InvalidArgument, "image required (set workflow `container:` or `runs-on: hyperfleet:hyperfleet://<image>`)")
	}
	body, err := json.Marshal(createBody{Image: image})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal create: %v", err)
	}
	httpReq, err := s.newRequest(ctx, http.MethodPost, "/machines", bytes.NewReader(body))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := s.do(httpReq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create machine: %v", err)
	}
	defer resp.Body.Close()
	var dto struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		return nil, status.Errorf(codes.Internal, "decode create response: %v", err)
	}
	if dto.ID == "" {
		return nil, status.Errorf(codes.Internal, "hyperfleet returned no id")
	}

	// Poll until the daemon reports running, or fail with the daemon's error.
	pollCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := s.waitRunning(pollCtx, dto.ID); err != nil {
		// Best-effort cleanup of the half-baked VM. Errors here are logged
		// to stderr (the runner pipes plugin stderr into its own log).
		_ = s.deleteMachine(context.Background(), dto.ID)
		return nil, err
	}

	return &pluginv1.CreateResponse{EnvironmentId: dto.ID}, nil
}

func (s *server) waitRunning(ctx context.Context, id string) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		req, err := s.newRequest(ctx, http.MethodGet, "/machines/"+url.PathEscape(id), nil)
		if err != nil {
			return status.Errorf(codes.Internal, "build poll request: %v", err)
		}
		resp, err := s.do(req)
		if err != nil {
			return status.Errorf(codes.Internal, "poll machine: %v", err)
		}
		var dto struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&dto)
		resp.Body.Close()
		switch dto.Status {
		case "running":
			return nil
		case "failed", "exited":
			return status.Errorf(codes.Internal, "machine entered %s: %s", dto.Status, dto.Error)
		}
		select {
		case <-ctx.Done():
			return status.Errorf(codes.DeadlineExceeded, "machine never became running: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Start is a no-op for hyperfleet: the daemon transitions the VM from
// pending → running before Create returns. We still wait on the in-guest
// initd's healthz here so any race between status=running and the vsock
// listener being up is hidden from the runner.
func (s *server) Start(ctx context.Context, req *pluginv1.StartRequest) (*pluginv1.StartResponse, error) {
	id := req.GetEnvironmentId()
	if id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "environment_id required")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, err := s.healthCheck(ctx, id)
		if err == nil && ok {
			break
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = errors.New("not ready")
			}
			return nil, status.Errorf(codes.Unavailable, "initd not ready: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil, status.Errorf(codes.DeadlineExceeded, "initd readiness wait: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	return &pluginv1.StartResponse{}, nil
}

func (s *server) healthCheck(ctx context.Context, id string) (bool, error) {
	req, err := s.newRequest(ctx, http.MethodGet, "/machines/"+url.PathEscape(id)+"/healthz", nil)
	if err != nil {
		return false, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if s.apiKey != "" {
		// only authoritative if not 401
	}
	return resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK, nil
}

func (s *server) Exec(req *pluginv1.ExecRequest, stream pluginv1.BackendPlugin_ExecServer) error {
	if req.GetEnvironmentId() == "" {
		return status.Errorf(codes.InvalidArgument, "environment_id required")
	}
	if len(req.GetCommand()) == 0 {
		return status.Errorf(codes.InvalidArgument, "command required")
	}

	body := struct {
		Command []string          `json:"command"`
		Env     map[string]string `json:"env,omitempty"`
		Workdir string            `json:"workdir,omitempty"`
		User    string            `json:"user,omitempty"`
	}{
		Command: req.GetCommand(),
		Env:     req.GetEnv(),
		Workdir: req.GetWorkdir(),
		User:    req.GetUser(),
	}
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return status.Errorf(codes.Internal, "encode exec: %v", err)
	}

	httpReq, err := s.newRequest(stream.Context(), http.MethodPost,
		"/machines/"+url.PathEscape(req.GetEnvironmentId())+"/exec", buf)
	if err != nil {
		return status.Errorf(codes.Internal, "build exec request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.ContentLength = int64(buf.Len())

	resp, err := s.http.Do(httpReq)
	if err != nil {
		return status.Errorf(codes.Internal, "exec http: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return status.Errorf(codes.Internal, "exec: %s: %s", resp.Status, body)
	}

	// Stream frames; relay each to the runner.
	hdr := make([]byte, frameHeaderSize)
	for {
		if _, err := io.ReadFull(resp.Body, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				return status.Errorf(codes.Internal, "exec: stream ended before exit frame")
			}
			return status.Errorf(codes.Internal, "exec frame header: %v", err)
		}
		kind := hdr[0]
		length := binary.BigEndian.Uint32(hdr[1:5])
		payload := make([]byte, length)
		if length > 0 {
			if _, err := io.ReadFull(resp.Body, payload); err != nil {
				return status.Errorf(codes.Internal, "exec frame payload: %v", err)
			}
		}
		switch kind {
		case frameStdout:
			if err := stream.Send(&pluginv1.ExecOutput{
				Stream: pluginv1.ExecOutput_STDOUT,
				Data:   payload,
			}); err != nil {
				return err
			}
		case frameStderr:
			if err := stream.Send(&pluginv1.ExecOutput{
				Stream: pluginv1.ExecOutput_STDERR,
				Data:   payload,
			}); err != nil {
				return err
			}
		case frameExit:
			if length != 4 {
				return status.Errorf(codes.Internal, "exec: bad exit payload len %d", length)
			}
			code := int32(binary.BigEndian.Uint32(payload))
			return stream.Send(&pluginv1.ExecOutput{
				Done:     true,
				ExitCode: code,
			})
		case frameError:
			return stream.Send(&pluginv1.ExecOutput{
				Done:         true,
				ExitCode:     1,
				ErrorMessage: string(payload),
			})
		default:
			return status.Errorf(codes.Internal, "exec: unknown frame kind %d", kind)
		}
	}
}

// CopyIn buffers the streamed tar into memory then PUTs it. The runner
// streams chunks of <=256 KiB; total tar size is bounded by the workspace
// it's copying. For very large workspaces we'd want to switch to a streaming
// pipe, but in v0 this matches existing backends' simplicity.
func (s *server) CopyIn(stream pluginv1.BackendPlugin_CopyInServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "copyin recv: %v", err)
	}
	envID := first.GetEnvironmentId()
	dest := first.GetDestPath()
	if envID == "" || dest == "" {
		return status.Errorf(codes.InvalidArgument, "environment_id and dest_path required on first chunk")
	}

	var buf bytes.Buffer
	if len(first.GetData()) > 0 {
		buf.Write(first.GetData())
	}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "copyin recv: %v", err)
		}
		if chunk.GetEnvironmentId() != "" || chunk.GetDestPath() != "" {
			return status.Errorf(codes.InvalidArgument, "environment_id/dest_path only on first chunk")
		}
		buf.Write(chunk.GetData())
	}

	if err := s.putTar(stream.Context(), envID, dest, &buf, int64(buf.Len())); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	return stream.SendAndClose(&pluginv1.CopyInResponse{})
}

// CopyLocal tars `src_path` on the host (where the runner is) and PUTs it
// into the guest. Only used when Capabilities.SupportsLocalCopy=true.
func (s *server) CopyLocal(ctx context.Context, req *pluginv1.CopyLocalRequest) (*pluginv1.CopyLocalResponse, error) {
	if req.GetEnvironmentId() == "" || req.GetSrcPath() == "" || req.GetDestPath() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "environment_id, src_path, dest_path required")
	}

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		errCh <- tarDir(pw, req.GetSrcPath())
	}()

	// Tar size is unknown; PUT with chunked encoding by passing -1.
	if err := s.putTar(ctx, req.GetEnvironmentId(), req.GetDestPath(), pr, -1); err != nil {
		_ = pr.CloseWithError(err)
		<-errCh
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if err := <-errCh; err != nil {
		return nil, status.Errorf(codes.Internal, "tar src: %v", err)
	}
	return &pluginv1.CopyLocalResponse{}, nil
}

func (s *server) CopyOut(req *pluginv1.CopyOutRequest, stream pluginv1.BackendPlugin_CopyOutServer) error {
	httpReq, err := s.newRequest(stream.Context(), http.MethodGet,
		"/machines/"+url.PathEscape(req.GetEnvironmentId())+"/files?path="+url.QueryEscape(req.GetSrcPath()), nil)
	if err != nil {
		return status.Errorf(codes.Internal, "build copyout: %v", err)
	}
	resp, err := s.do(httpReq)
	if err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 256*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&pluginv1.CopyOutChunk{Data: append([]byte(nil), buf[:n]...)}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "copyout body: %v", err)
		}
	}
}

func (s *server) UpdateEnv(ctx context.Context, req *pluginv1.UpdateEnvRequest) (*pluginv1.UpdateEnvResponse, error) {
	current := req.GetCurrentEnv()
	if current == nil {
		current = make(map[string]string)
	}

	httpReq, err := s.newRequest(ctx, http.MethodGet,
		"/machines/"+url.PathEscape(req.GetEnvironmentId())+"/files?path="+url.QueryEscape(req.GetSrcPath()), nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build updateenv: %v", err)
	}
	resp, err := s.http.Do(httpReq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "updateenv http: %v", err)
	}
	defer resp.Body.Close()
	// Missing env file: not an error, just return the current env unchanged.
	if resp.StatusCode == http.StatusInternalServerError {
		return &pluginv1.UpdateEnvResponse{UpdatedEnv: current}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return &pluginv1.UpdateEnvResponse{UpdatedEnv: current}, nil
	}

	tr := tar.NewReader(resp.Body)
	hdr, err := tr.Next()
	if err != nil {
		// empty tar — leave current as-is
		return &pluginv1.UpdateEnvResponse{UpdatedEnv: current}, nil
	}
	if hdr.Typeflag != tar.TypeReg {
		return &pluginv1.UpdateEnvResponse{UpdatedEnv: current}, nil
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read env file: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			current[line[:i]] = line[i+1:]
		}
	}
	return &pluginv1.UpdateEnvResponse{UpdatedEnv: current}, nil
}

func (s *server) IsHealthy(ctx context.Context, req *pluginv1.IsHealthyRequest) (*pluginv1.IsHealthyResponse, error) {
	ok, err := s.healthCheck(ctx, req.GetEnvironmentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "healthz: %v", err)
	}
	if !ok {
		return nil, status.Errorf(codes.Unavailable, "initd not ready")
	}
	return &pluginv1.IsHealthyResponse{}, nil
}

func (s *server) Remove(ctx context.Context, req *pluginv1.RemoveRequest) (*pluginv1.RemoveResponse, error) {
	if err := s.deleteMachine(ctx, req.GetEnvironmentId()); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &pluginv1.RemoveResponse{}, nil
}

func (s *server) deleteMachine(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	req, err := s.newRequest(ctx, http.MethodDelete, "/machines/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("delete machine: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete machine: %s", resp.Status)
	}
	return nil
}

// ---- helpers ----

func (s *server) putTar(ctx context.Context, envID, dest string, body io.Reader, contentLength int64) error {
	req, err := s.newRequest(ctx, http.MethodPut,
		"/machines/"+url.PathEscape(envID)+"/files?path="+url.QueryEscape(dest), body)
	if err != nil {
		return fmt.Errorf("build put: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("put tar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("put tar: %s: %s", resp.Status, body)
	}
	return nil
}

// tarDir writes a tar of dir's contents to w. Entries are stored relative to
// dir (so consumers can extract under any destination path).
func tarDir(w io.WriteCloser, dir string) error {
	defer w.Close()
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			l, err := os.Readlink(path)
			if err != nil {
				return err
			}
			link = l
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

package initd

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client speaks the in-guest HTTP/1.1 control plane over a Firecracker vsock
// AF_UNIX socket. The host dials the UDS, sends "CONNECT <port>\n", reads
// the firecracker handshake reply, then drives a normal HTTP/1.1 conversation.
//
// One Client wraps a UDS path; each request opens a fresh tunneled connection
// (cheap: it's local, no TLS, and the connection is short-lived). Concurrent
// callers are safe.
type Client struct {
	udsPath string
	port    uint32
	// dialTimeout caps the time spent on the unix dial + CONNECT handshake.
	// Set per-request via WithTimeout if a different bound is needed.
	dialTimeout time.Duration
	httpClient  *http.Client
}

// New returns a client against the given firecracker vsock UDS path. Port is
// the in-guest vsock listener port (VsockPort in protocol.go for the standard
// initd build).
func New(udsPath string, port uint32) *Client {
	c := &Client{
		udsPath:     udsPath,
		port:        port,
		dialTimeout: 5 * time.Second,
	}
	c.httpClient = &http.Client{
		Transport: &http.Transport{
			// Each call gets its own tunneled conn. Disabling keepalives
			// avoids a stuck pooled conn after the guest reboots between
			// jobs.
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return c.dialVsock(ctx)
			},
		},
	}
	return c
}

// dialVsock opens the UDS, writes the CONNECT line, and consumes
// firecracker's "OK <port>\n" reply. Returns the live connection on success.
func (c *Client) dialVsock(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: c.dialTimeout}
	conn, err := d.DialContext(ctx, "unix", c.udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.udsPath, err)
	}
	// Apply a deadline only for the handshake; clear it once we hand the
	// conn back to net/http.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(c.dialTimeout))
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", c.port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT reply: %w", err)
	}
	if len(line) < 2 || line[:2] != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %q", line)
	}
	_ = conn.SetDeadline(time.Time{})
	// If firecracker buffered any bytes ahead, hand them back via a
	// thin wrapper so net/http sees them on the first Read.
	if br.Buffered() > 0 {
		buf := make([]byte, br.Buffered())
		_, _ = io.ReadFull(br, buf)
		return &prefixedConn{Conn: conn, prefix: buf}, nil
	}
	return conn, nil
}

type prefixedConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixedConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// Healthz pings GET /healthz with a short timeout. Used by the daemon to
// know when the in-guest initd is ready to accept Exec.
func (c *Client) Healthz(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://initd/healthz", nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// WaitReady polls Healthz until success or ctx expires. The kernel boot,
// initd startup, and vsock listener creation usually complete in <1 s, but
// pulling images on first run can lag and we'd rather not race.
func (c *Client) WaitReady(ctx context.Context) error {
	backoff := 50 * time.Millisecond
	for {
		if err := c.Healthz(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 500*time.Millisecond {
			backoff *= 2
		}
	}
}

// ExecResult is what the framed Exec stream collapses to once the response
// has been fully consumed. ExitCode is the int32 sent in FrameExit; Error,
// when non-empty, indicates the guest could not run the command at all
// (FrameError, mutually exclusive with FrameExit).
type ExecResult struct {
	ExitCode int32
	Error    string
}

// Exec runs a command in the guest, fanning the framed response into the
// caller-provided stdout / stderr writers. nil writers discard. Returns
// once the guest emits FrameExit or FrameError; other framing errors are
// returned as a Go error and the exec is considered to have failed.
func (c *Client) Exec(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (ExecResult, error) {
	body, err := jsonBody(req)
	if err != nil {
		return ExecResult{}, fmt.Errorf("encode exec body: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://initd/exec", body)
	if err != nil {
		return ExecResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.ContentLength = int64(body.Len())

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return ExecResult{}, fmt.Errorf("post /exec: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ExecResult{}, fmt.Errorf("/exec: %s", resp.Status)
	}

	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	br := bufio.NewReader(resp.Body)
	hdrBuf := make([]byte, FrameHeaderSize)
	for {
		if _, err := io.ReadFull(br, hdrBuf); err != nil {
			if errors.Is(err, io.EOF) {
				return ExecResult{}, fmt.Errorf("/exec: stream ended before exit frame")
			}
			return ExecResult{}, fmt.Errorf("/exec frame header: %w", err)
		}
		hdr := DecodeHeader(hdrBuf)
		payload := make([]byte, hdr.Length)
		if hdr.Length > 0 {
			if _, err := io.ReadFull(br, payload); err != nil {
				return ExecResult{}, fmt.Errorf("/exec frame payload: %w", err)
			}
		}
		switch hdr.Kind {
		case FrameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return ExecResult{}, fmt.Errorf("write stdout: %w", err)
			}
		case FrameStderr:
			if _, err := stderr.Write(payload); err != nil {
				return ExecResult{}, fmt.Errorf("write stderr: %w", err)
			}
		case FrameExit:
			if hdr.Length != 4 {
				return ExecResult{}, fmt.Errorf("/exec: bad exit payload len %d", hdr.Length)
			}
			return ExecResult{ExitCode: int32(binary.BigEndian.Uint32(payload))}, nil
		case FrameError:
			return ExecResult{Error: string(payload)}, nil
		default:
			return ExecResult{}, fmt.Errorf("/exec: unknown frame kind %d", hdr.Kind)
		}
	}
}

// PutTar streams a tar archive to PUT /tar?path=<dest>. The guest extracts
// it under dest (creating dest if missing). dest must be an absolute path.
func (c *Client) PutTar(ctx context.Context, dest string, body io.Reader, contentLength int64) error {
	if !isAbs(dest) {
		return fmt.Errorf("dest must be absolute: %q", dest)
	}
	u := "http://initd/tar?path=" + url.QueryEscape(dest)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body)
	if err != nil {
		return err
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("put /tar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("put /tar: %s: %s", resp.Status, string(body))
	}
	return nil
}

// GetTar opens a tar stream of the guest path. Caller must close the
// returned reader; doing so cancels the underlying request.
func (c *Client) GetTar(ctx context.Context, src string) (io.ReadCloser, error) {
	if !isAbs(src) {
		return nil, fmt.Errorf("src must be absolute: %q", src)
	}
	u := "http://initd/tar?path=" + url.QueryEscape(src)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get /tar: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("get /tar: %s: %s", resp.Status, string(body))
	}
	return resp.Body, nil
}

// Stat returns the parsed StatResponse for src.
func (c *Client) Stat(ctx context.Context, src string) (StatResponse, error) {
	if !isAbs(src) {
		return StatResponse{}, fmt.Errorf("src must be absolute: %q", src)
	}
	u := "http://initd/stat?path=" + url.QueryEscape(src)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return StatResponse{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return StatResponse{}, fmt.Errorf("get /stat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return StatResponse{}, fmt.Errorf("get /stat: %s", resp.Status)
	}
	var out StatResponse
	if err := decodeJSON(resp.Body, &out); err != nil {
		return StatResponse{}, fmt.Errorf("decode /stat: %w", err)
	}
	return out, nil
}

// formatPort is exported only so tests can introspect the connect line.
func formatPort(p uint32) string { return strconv.FormatUint(uint64(p), 10) }

func isAbs(p string) bool { return len(p) > 0 && p[0] == '/' }

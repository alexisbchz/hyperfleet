package initd

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeInitd serves a forced-frame /exec response so the client can be
// exercised without a guest. We bypass the vsock dialer by pointing the
// client at an httptest server via DialContext.
type fakeInitd struct {
	*httptest.Server
}

func newFakeInitd(handler http.Handler) *fakeInitd {
	return &fakeInitd{httptest.NewServer(handler)}
}

func (f *fakeInitd) httpClient() *http.Client {
	addr := strings.TrimPrefix(f.URL, "http://")
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", addr)
			},
		},
	}
}

// newClientWithHTTP builds a *Client whose http transport is wired to the
// given httptest server, sidestepping the unix dial + CONNECT handshake.
func newClientWithHTTP(httpc *http.Client) *Client {
	return &Client{
		udsPath:    "(test)",
		port:       VsockPort,
		httpClient: httpc,
	}
}

func writeFrame(w io.Writer, kind FrameKind, payload []byte) error {
	hdr := make([]byte, FrameHeaderSize)
	EncodeHeader(hdr, FrameHeader{Kind: kind, Length: uint32(len(payload))})
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func TestExecParsesFrames(t *testing.T) {
	srv := newFakeInitd(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exec" || r.Method != http.MethodPost {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_ = writeFrame(w, FrameStdout, []byte("hello\n"))
		_ = writeFrame(w, FrameStderr, []byte("oops\n"))
		exit := make([]byte, 4)
		binary.BigEndian.PutUint32(exit, uint32(int32(7)))
		_ = writeFrame(w, FrameExit, exit)
	}))
	defer srv.Close()

	c := newClientWithHTTP(srv.httpClient())

	var stdout, stderr bytes.Buffer
	res, err := c.Exec(context.Background(), ExecRequest{Command: []string{"true"}}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}
	if got := stdout.String(); got != "hello\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := stderr.String(); got != "oops\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestExecCarriesError(t *testing.T) {
	srv := newFakeInitd(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_ = writeFrame(w, FrameError, []byte("exec failed: ENOENT"))
	}))
	defer srv.Close()

	c := newClientWithHTTP(srv.httpClient())
	res, err := c.Exec(context.Background(), ExecRequest{Command: []string{"missing"}}, nil, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Error != "exec failed: ENOENT" {
		t.Errorf("Error = %q", res.Error)
	}
}

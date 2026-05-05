package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/alexis-bouchez/hyperfleet/internal/initd"
	"github.com/alexis-bouchez/hyperfleet/internal/vmmgr"
	"github.com/go-chi/chi/v5"
)

// RegisterInitdRoutes wires the streaming endpoints that proxy through to
// each VM's in-guest initd over vsock. Kept off the typed huma surface
// because exec/tar bodies are streamed and would otherwise force buffering.
//
// Routes (rooted at /machines/{id}):
//
//	POST /exec       — body: ExecRequest JSON. Response: framed stream
//	                   identical to what initd emits (5-byte header + payload).
//	PUT  /files?path=ABS — body: tar archive; extracted under path in guest.
//	GET  /files?path=ABS — body: tar archive of guest path.
//	GET  /stat?path=ABS  — body: StatResponse JSON.
//	GET  /healthz        — 204 once the in-guest initd answers.
func RegisterInitdRoutes(r chi.Router, mgr *vmmgr.Manager) {
	h := &initdHandler{mgr: mgr}
	r.Post("/machines/{id}/exec", h.exec)
	r.Put("/machines/{id}/files", h.putFiles)
	r.Get("/machines/{id}/files", h.getFiles)
	r.Get("/machines/{id}/stat", h.stat)
	r.Get("/machines/{id}/healthz", h.healthz)
}

type initdHandler struct {
	mgr *vmmgr.Manager
}

func (h *initdHandler) client(w http.ResponseWriter, r *http.Request) (*initd.Client, bool) {
	id := chi.URLParam(r, "id")
	c, err := h.mgr.InitdClient(id)
	if err != nil {
		if errors.Is(err, vmmgr.ErrNotFound) {
			http.Error(w, "machine not found", http.StatusNotFound)
			return nil, false
		}
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return nil, false
	}
	return c, true
}

func (h *initdHandler) exec(w http.ResponseWriter, r *http.Request) {
	c, ok := h.client(w, r)
	if !ok {
		return
	}
	var req initd.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Command) == 0 {
		http.Error(w, "command required", http.StatusBadRequest)
		return
	}

	// Re-emit the framed stream verbatim by piping each frame through.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	pr, pw := io.Pipe()
	defer pr.Close()

	doneCh := make(chan execDone, 1)
	go func() {
		res, err := c.Exec(r.Context(), req, frameWriter{w: pw, kind: initd.FrameStdout},
			frameWriter{w: pw, kind: initd.FrameStderr})
		_ = pw.Close()
		doneCh <- execDone{res: res, err: err}
	}()

	// Pump frames from the pipe writer (stdout/stderr framing) into the
	// HTTP response, flushing after each chunk so the client gets
	// near-realtime output.
	buf := make([]byte, 32*1024)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}

	res := <-doneCh
	if res.err != nil {
		writeFrame(w, initd.FrameError, []byte(res.err.Error()))
	} else if res.res.Error != "" {
		writeFrame(w, initd.FrameError, []byte(res.res.Error))
	} else {
		exit := make([]byte, 4)
		exit[0] = byte(uint32(res.res.ExitCode) >> 24)
		exit[1] = byte(uint32(res.res.ExitCode) >> 16)
		exit[2] = byte(uint32(res.res.ExitCode) >> 8)
		exit[3] = byte(uint32(res.res.ExitCode))
		writeFrame(w, initd.FrameExit, exit)
	}
	if flusher != nil {
		flusher.Flush()
	}
}

type execDone struct {
	res initd.ExecResult
	err error
}

// frameWriter wraps an io.Writer with our 5-byte frame header on every
// Write call. Used to splice initd.Client's stdout/stderr writers into a
// single io.Pipe that the HTTP response can drain.
type frameWriter struct {
	w    io.Writer
	kind initd.FrameKind
}

func (f frameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	hdr := make([]byte, initd.FrameHeaderSize)
	initd.EncodeHeader(hdr, initd.FrameHeader{Kind: f.kind, Length: uint32(len(p))})
	if _, err := f.w.Write(hdr); err != nil {
		return 0, err
	}
	if _, err := f.w.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func writeFrame(w io.Writer, kind initd.FrameKind, payload []byte) {
	hdr := make([]byte, initd.FrameHeaderSize)
	initd.EncodeHeader(hdr, initd.FrameHeader{Kind: kind, Length: uint32(len(payload))})
	_, _ = w.Write(hdr)
	if len(payload) > 0 {
		_, _ = w.Write(payload)
	}
}

func (h *initdHandler) putFiles(w http.ResponseWriter, r *http.Request) {
	c, ok := h.client(w, r)
	if !ok {
		return
	}
	dest, ok := absQuery(w, r, "path")
	if !ok {
		return
	}
	if err := c.PutTar(r.Context(), dest, r.Body, r.ContentLength); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *initdHandler) getFiles(w http.ResponseWriter, r *http.Request) {
	c, ok := h.client(w, r)
	if !ok {
		return
	}
	src, ok := absQuery(w, r, "path")
	if !ok {
		return
	}
	rc, err := c.GetTar(r.Context(), src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (h *initdHandler) stat(w http.ResponseWriter, r *http.Request) {
	c, ok := h.client(w, r)
	if !ok {
		return
	}
	src, ok := absQuery(w, r, "path")
	if !ok {
		return
	}
	out, err := c.Stat(r.Context(), src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *initdHandler) healthz(w http.ResponseWriter, r *http.Request) {
	c, ok := h.client(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.Healthz(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func absQuery(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	v, err := url.QueryUnescape(r.URL.Query().Get(key))
	if err != nil || v == "" || v[0] != '/' {
		http.Error(w, fmt.Sprintf("query %q must be an absolute path", key), http.StatusBadRequest)
		return "", false
	}
	return v, true
}

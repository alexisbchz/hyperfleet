// Package initd defines the host↔guest control-plane protocol used between
// hyperfleet's daemon and the in-guest init binary (cmd/initd).
//
// Wire model: the daemon owns one Firecracker-managed AF_UNIX socket per VM
// (firecracker writes vsock packets to it). The daemon dials that socket and
// writes "CONNECT <port>\n" to ask the in-guest VMM to forward to a vsock
// listener. From there the connection is plain HTTP/1.1 — easy to debug with
// `curl --unix-socket`-style tools, no extra deps in the guest image.
//
// Endpoints (all rooted at the vsock listener):
//
//	POST /exec    — body: ExecRequest (JSON). Response: framed stream (see Frame*).
//	PUT  /tar     — query: ?path=<abs>. Body: tar archive. 204 on success.
//	GET  /tar     — query: ?path=<abs>. Body: tar archive of file or directory.
//	GET  /stat    — query: ?path=<abs>. Body: StatResponse (JSON).
//
// The Exec response is intentionally not JSON: each step's stdout/stderr can
// be many MB and we want to stream it without buffering. Frames are written
// back-to-back on the response body until a terminal Exit frame.
package initd

import "encoding/binary"

// VsockPort is the AF_VSOCK port the initd listens on inside the guest.
// Picked above the 1024 reserved range, well below firecracker's typical
// dynamic allocations.
const VsockPort = 1024

// FrameKind tags each frame on the Exec response stream.
type FrameKind uint8

const (
	FrameStdout FrameKind = 1
	FrameStderr FrameKind = 2
	// FrameExit is the terminal frame. Payload is a 4-byte big-endian int32
	// exit code; on signal-kill or spawn failure, ExecError follows in a
	// trailing FrameError frame instead.
	FrameExit FrameKind = 3
	// FrameError carries a UTF-8 error message when the guest could not run
	// the command at all (e.g. exec failed before fork). Mutually exclusive
	// with FrameExit.
	FrameError FrameKind = 4
)

// FrameHeader is written before each frame's payload:
//
//	[1 byte Kind][4 bytes BE length][N bytes payload]
//
// Length is in bytes and may be zero (empty stdout chunk is legal but useless;
// FrameExit with len=4 carries the exit code; FrameError with len>0 carries
// the message).
type FrameHeader struct {
	Kind   FrameKind
	Length uint32
}

const FrameHeaderSize = 5

// EncodeHeader writes h into a 5-byte slice. Caller owns buf.
func EncodeHeader(buf []byte, h FrameHeader) {
	_ = buf[FrameHeaderSize-1] // bounds check hint
	buf[0] = byte(h.Kind)
	binary.BigEndian.PutUint32(buf[1:5], h.Length)
}

// DecodeHeader is the inverse of EncodeHeader. Caller owns buf.
func DecodeHeader(buf []byte) FrameHeader {
	_ = buf[FrameHeaderSize-1]
	return FrameHeader{
		Kind:   FrameKind(buf[0]),
		Length: binary.BigEndian.Uint32(buf[1:5]),
	}
}

// ExecRequest is the JSON body of POST /exec.
type ExecRequest struct {
	// Command is the argv to run inside the guest. Command[0] is resolved
	// against $PATH unless absolute.
	Command []string `json:"command"`
	// Env is overlaid on top of the initd's process env. Use empty-string
	// values to unset a variable inherited from initd.
	Env map[string]string `json:"env,omitempty"`
	// Workdir is an absolute path; the guest creates it if missing.
	// Empty defaults to "/".
	Workdir string `json:"workdir,omitempty"`
	// User is reserved for future uid/gid switching; ignored in v0.
	User string `json:"user,omitempty"`
}

// StatResponse is the JSON body of GET /stat.
type StatResponse struct {
	Exists bool   `json:"exists"`
	IsDir  bool   `json:"isDir,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

package initd

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeHeader(t *testing.T) {
	cases := []FrameHeader{
		{Kind: FrameStdout, Length: 0},
		{Kind: FrameStderr, Length: 7},
		{Kind: FrameExit, Length: 4},
		{Kind: FrameError, Length: 1<<31 - 1},
	}
	buf := make([]byte, FrameHeaderSize)
	for _, c := range cases {
		EncodeHeader(buf, c)
		got := DecodeHeader(buf)
		if got != c {
			t.Errorf("roundtrip %v -> %v", c, got)
		}
	}
}

func TestEncodeHeaderBigEndian(t *testing.T) {
	buf := make([]byte, FrameHeaderSize)
	EncodeHeader(buf, FrameHeader{Kind: FrameExit, Length: 0x01020304})
	want := []byte{byte(FrameExit), 0x01, 0x02, 0x03, 0x04}
	if !bytes.Equal(buf, want) {
		t.Errorf("bytes = %v want %v", buf, want)
	}
}

package proto

import (
	"bytes"
	"testing"
)

// FuzzReadFrame feeds arbitrary bytes to the wire parser. The parser is the
// first thing that touches guest-supplied data — it must NEVER panic,
// allocate beyond caps, or accept frames larger than MaxFrameSize.
func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x01, 0x00, 0x00, 0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add(func() []byte {
		b := make([]byte, 13)
		b[0] = 0x01
		b[6] = 0xff // huge declared length
		return b
	}())
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		_, _ = ReadFrame(r) // must not panic regardless of input
	})
}

// FuzzFrameEcho: whatever parses cleanly must survive a write/read roundtrip
// without corrupting the stream framing.
func FuzzFrameEcho(f *testing.F) {
	f.Add(uint32(1), uint8(0), []byte("hello"))
	f.Add(uint32(0xffffffff), uint8(0xff), []byte{})
	f.Fuzz(func(t *testing.T, sid uint32, op byte, payload []byte) {
		if len(payload) > 1024 {
			payload = payload[:1024]
		}
		var buf bytes.Buffer
		in := Frame{StreamID: sid, Flags: FlagBinary, Op: OpExecData, Payload: payload}
		if err := WriteFrame(&buf, in); err != nil {
			t.Skip()
		}
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("valid frame failed to parse: %v", err)
		}
		if got.StreamID != in.StreamID || got.Op != in.Op || !bytes.Equal(got.Payload, in.Payload) {
			t.Fatalf("roundtrip corrupted: %+v != %+v", got, in)
		}
	})
}

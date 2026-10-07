package proto

import (
	"bytes"
	"net"
	"testing"
)

func BenchmarkFrameCodec(b *testing.B) {
	f := Frame{StreamID: 42, Flags: FlagBinary, Op: OpExecData, Payload: make([]byte, 8192)}
	b.SetBytes(int64(len(f.Payload)))
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, f); err != nil {
			b.Fatal(err)
		}
		if _, err := ReadFrame(&buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMuxRoundTrip(b *testing.B) {
	c1, c2 := net.Pipe()
	host := NewMux(c1, true)
	guest := NewMux(c2, false)

	go func() {
		for {
			st, err := guest.Accept()
			if err != nil {
				return
			}
			go func(s *Stream) {
				for {
					f, err := s.Recv()
					if err != nil {
						return
					}
					s.SendBinary(OpExecData, f.Payload)
				}
			}(st)
		}
	}()

	payload := make([]byte, 4096)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := host.Open(OpExecData, payload)
		if err != nil {
			b.Fatal(err)
		}
		f, err := st.Recv()
		if err != nil || len(f.Payload) != len(payload) {
			b.Fatal("roundtrip failed")
		}
		st.Close()
	}
}

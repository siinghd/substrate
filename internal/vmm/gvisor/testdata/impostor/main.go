// Command impostor is guest code that tries to be the sandbox's agent.
//
// The host keeps its agent-socket listener open so the real agent can reconnect
// after a transport error, and that socket is bind-mounted into the sandbox --
// so anything running inside can dial it. If the host accepted such a
// connection it would take exec and fs calls from tenant code and, because the
// agent is what reports the sandbox's CPU and memory, would let a tenant
// under-report the usage it is billed for.
//
// It uses the REAL protocol encoder. A hand-rolled frame would be rejected for
// being malformed, which would make this test pass for the wrong reason.
package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/substrate-sh/substrate/internal/proto"
)

func main() {
	path := os.Args[1]
	deadline := time.Now().Add(12 * time.Second)
	attempts, talked := 0, 0
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		attempts++
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		mux := proto.NewMux(conn, false)
		hello := proto.Hello{
			Proto: 1, Agent: "substrate-shim", Version: "1.0.0",
			BootID: "forged", Features: []string{"exec", "fs"},
			Token:  "0000000000000000000000000000000000000000000000000000000000000000",
		}
		if st, err := mux.Open(proto.OpHello, proto.Encode(hello)); err == nil {
			st.Close()
			// If the host accepted us as the agent it stays connected and
			// starts sending work. Anything we can read back means we were
			// not dropped.
			if _, rerr := mux.Accept(); rerr == nil {
				talked++
			}
		}
		conn.Close()
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Printf("IMPOSTOR attempts=%d accepted=%d\n", attempts, talked)
}

// Command minisite is a tenant's web server, for proving a sandbox can host
// one that the outside world can reach.
//
// Staged as a static binary rather than installed with a package manager: the
// point of this test is the network path into the sandbox, and a CDN outage
// should not be able to make it look like that path is broken.
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	addr := ":8080"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<h1>hello from the sandbox</h1>")
	})
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
}

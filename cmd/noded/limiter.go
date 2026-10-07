package main

import (
	"net"
	"net/http"
)

// limiterIdentity keys the pre-auth limiter on the PEER ADDRESS, never on the
// presented credential.
//
// The limiter runs before authentication, so the bearer token is attacker-
// chosen bytes: a fresh random token per request hashed to a fresh principal,
// minted a fresh bucket with a full burst, and let 100% of a flood through —
// each request still reaching GetAPIKeyByHash, so the limiter that exists to
// stop API-key brute force was funding one. The peer address is the only
// identity available at this point that the caller cannot mint at will.
//
// X-Forwarded-For is deliberately ignored: it is a client-supplied header, so
// trusting it would restore exactly the bypass this closes. Deployments behind
// a trusted proxy should terminate and re-inject a verified identity instead.
func limiterIdentity() func(*http.Request) string {
	return func(r *http.Request) string {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if host == "" {
			return "anon:"
		}
		return "ip_" + host
	}
}

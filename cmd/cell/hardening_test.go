package main

// Pass-4 hardening tests for the cell plane: fail-closed node secret and
// bounded request bodies.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNodeSecretFailClosed(t *testing.T) {
	cases := []struct {
		name       string
		nodeSecret string
		jwtSecret  string
		insecure   string
		wantErr    bool
	}{
		{"missing both", "", "", "", true},
		{"node secret ok", strings.Repeat("n", 32), "", "", false},
		{"jwt fallback ok", "", strings.Repeat("j", 32), "", false},
		{"too short", "short", "", "", true},
		{"short allowed in insecure dev", "short", "", "1", false},
	}
	for _, c := range cases {
		t.Setenv("SUBSTRATE_NODE_SECRET", c.nodeSecret)
		t.Setenv("SUBSTRATE_JWT_SECRET", c.jwtSecret)
		t.Setenv("SUBSTRATE_INSECURE_DEV", c.insecure)
		_, err := resolveNodeSecret()
		if c.wantErr && err == nil {
			t.Fatalf("%s: expected error, got none", c.name)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
	}
}

func TestBodyLimitRejectsOversized(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := readAll(r)
		if err != nil {
			http.Error(w, "body too large or unreadable", http.StatusBadRequest)
			return
		}
		w.WriteHeader(200)
		w.Write(body)
	})
	srv := httptest.NewServer(bodyLimit(handler))
	defer srv.Close()

	huge := bytes.Repeat([]byte("a"), maxCellBody+1)
	resp, err := http.Post(srv.URL, "application/octet-stream", bytes.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body accepted with status %d", resp.StatusCode)
	}

	// Control: a normal body passes.
	resp2, err := http.Post(srv.URL, "application/octet-stream", strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("normal body rejected with status %d", resp2.StatusCode)
	}
}

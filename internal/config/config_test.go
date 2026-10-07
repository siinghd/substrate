package config

import "testing"

// Dev mode must never run with an EMPTY or well-known JWT secret: Load
// generates an ephemeral secret instead (production still refuses to boot).
func TestInsecureDevGetsEphemeralSecret(t *testing.T) {
	t.Setenv("SUBSTRATE_INSECURE_DEV", "1")
	t.Setenv("SUBSTRATE_JWT_SECRET", "")
	cfg, err := Load("-")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.JWTSecret == "" {
		t.Fatal("dev boot left JWT secret empty: sandbox tokens would be forgeable")
	}
	if len(cfg.API.JWTSecret) < 32 {
		t.Fatalf("dev secret too short: %d", len(cfg.API.JWTSecret))
	}

	// Each boot gets a DIFFERENT ephemeral secret.
	cfg2, err := Load("-")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.API.JWTSecret == cfg.API.JWTSecret {
		t.Fatal("dev secret is not ephemeral across boots")
	}

	// Production mode still fails closed on a missing secret.
	t.Setenv("SUBSTRATE_INSECURE_DEV", "")
	if _, err := Load("-"); err == nil {
		t.Fatal("production booted without SUBSTRATE_JWT_SECRET")
	}
}

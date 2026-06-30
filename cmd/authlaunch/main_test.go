package main

import (
	"os"
	"strings"
	"testing"
)

func TestOidcConfigured(t *testing.T) {
	full := oidcConfig{IssuerURL: "https://auth.example", ClientID: "brogue", AllowedGroup: "players"}
	if !(config{oidc: full}).oidcConfigured() {
		t.Error("all three OIDC vars set: want configured")
	}
	for _, tc := range []struct {
		name string
		oidc oidcConfig
	}{
		{"no issuer", oidcConfig{ClientID: full.ClientID, AllowedGroup: full.AllowedGroup}},
		{"no client", oidcConfig{IssuerURL: full.IssuerURL, AllowedGroup: full.AllowedGroup}},
		{"no group", oidcConfig{IssuerURL: full.IssuerURL, ClientID: full.ClientID}},
		{"none", oidcConfig{}},
	} {
		if (config{oidc: tc.oidc}).oidcConfigured() {
			t.Errorf("%s: want not configured", tc.name)
		}
	}
}

// loadConfig must plumb the Tailscale ingress + frontend-rewrite env vars onto
// the OIDC config (consumed by DeviceFlow), and default the ingress to "lan".
func TestLoadConfigPlumbsIngressRewrite(t *testing.T) {
	t.Setenv("OIDC_ISSUER_URL", "https://auth.example.com")
	t.Setenv("OIDC_CLIENT_ID", "brogue")
	t.Setenv("OIDC_ALLOWED_GROUP", "players")
	t.Setenv("BROGUE_INGRESS", "tailscale")
	t.Setenv("OIDC_FRONTEND_URL_TS", "https://auth.your-tailnet.ts.net")

	cfg, err := loadConfig([]string{"/bin/true"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.oidc.Ingress != "tailscale" {
		t.Errorf("Ingress = %q, want %q", cfg.oidc.Ingress, "tailscale")
	}
	if cfg.oidc.FrontendTS != "https://auth.your-tailnet.ts.net" {
		t.Errorf("FrontendTS = %q, want the ts.net base", cfg.oidc.FrontendTS)
	}
}

func TestLoadConfigIngressDefaultsToLAN(t *testing.T) {
	t.Setenv("OIDC_ISSUER_URL", "https://auth.example.com")
	t.Setenv("OIDC_CLIENT_ID", "brogue")
	t.Setenv("OIDC_ALLOWED_GROUP", "players")
	t.Setenv("BROGUE_INGRESS", "") // unset/empty must fall back to "lan"

	cfg, err := loadConfig([]string{"/bin/true"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.oidc.Ingress != "lan" {
		t.Errorf("Ingress = %q, want lan default", cfg.oidc.Ingress)
	}
}

// authenticate must refuse a session that carries no usable public key before
// it touches the network (the issuer URL here would fail discovery anyway —
// the error text proves we never got that far).
func TestAuthenticateRequiresPubkey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pubkey string
	}{
		{"no key", ""},
		{"garbage key", "not an authorized_keys line"},
	} {
		cfg := config{
			oidc:       oidcConfig{IssuerURL: "https://invalid.invalid", ClientID: "x", AllowedGroup: "g"},
			pubkeyLine: tc.pubkey,
		}
		_, err := authenticate(cfg, os.Stdout)
		if err == nil {
			t.Fatalf("%s: want error, got nil", tc.name)
		}
		if !strings.Contains(err.Error(), "no SSH public key") {
			t.Errorf("%s: want pubkey refusal, got: %v", tc.name, err)
		}
	}
}

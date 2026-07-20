package main

import "testing"

func TestResolveAddress_PassesThroughLiteralIPv4(t *testing.T) {
	got, err := resolveAddress("127.0.0.1:3000")
	if err != nil {
		t.Fatalf("resolveAddress: %v", err)
	}
	if got != "127.0.0.1:3000" {
		t.Fatalf("expected a literal IP to pass through unchanged, got %q", got)
	}
}

func TestResolveAddress_PassesThroughLiteralIPv6(t *testing.T) {
	got, err := resolveAddress("[::1]:3000")
	if err != nil {
		t.Fatalf("resolveAddress: %v", err)
	}
	if got != "[::1]:3000" {
		t.Fatalf("expected a literal IP to pass through unchanged, got %q", got)
	}
}

func TestResolveAddress_ResolvesHostnamePreferringIPv4(t *testing.T) {
	// "localhost" resolves via the hosts file/NSS rather than a real DNS
	// query, so this stays fast and deterministic in CI with no network
	// egress. It also exercises the IPv4-preference logic: localhost
	// typically resolves to both 127.0.0.1 and ::1, and resolveAddress
	// must pick the IPv4 one regardless of the order LookupIP returns them.
	got, err := resolveAddress("localhost:3000")
	if err != nil {
		t.Fatalf("resolveAddress: %v", err)
	}
	if got != "127.0.0.1:3000" {
		t.Fatalf("expected localhost to resolve to 127.0.0.1:3000, got %q", got)
	}
}

func TestResolveAddress_RejectsMissingPort(t *testing.T) {
	if _, err := resolveAddress("localhost"); err == nil {
		t.Fatal("expected an error for an address with no port")
	}
}

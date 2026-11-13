package main

import (
	"net"
	"strings"
	"testing"

	"github.com/kimoju/quectel-qmi-go/pkg/qmi"
)

// MEDIUM-7: cmd/cm -4 -6 dual-stack flag handling and IPv6-only / dual-stack status formatting.
func TestMedium7CMResolveIPFamiliesAndFormatConnectedIPs(t *testing.T) {
	if v4, v6 := resolveIPFamilies(false, false); !v4 || !v6 {
		t.Fatalf("resolveIPFamilies(false, false) = (%v, %v), want (true, true)", v4, v6)
	}
	if v4, v6 := resolveIPFamilies(true, false); !v4 || v6 {
		t.Fatalf("resolveIPFamilies(true, false) = (%v, %v), want (true, false)", v4, v6)
	}
	if v4, v6 := resolveIPFamilies(false, true); v4 || !v6 {
		t.Fatalf("resolveIPFamilies(false, true) = (%v, %v), want (false, true)", v4, v6)
	}
	if v4, v6 := resolveIPFamilies(true, true); !v4 || !v6 {
		t.Fatalf("resolveIPFamilies(true, true) = (%v, %v), want (true, true) for explicit dual-stack -4 -6", v4, v6)
	}

	v6Only := formatConnectedIPs(&qmi.RuntimeSettings{
		IPv6Address: net.ParseIP("2409:8088::1234"),
		IPv6Prefix:  64,
	})
	if strings.Contains(v6Only, "<nil>") || v6Only != "IPv6=2409:8088::1234/64" {
		t.Fatalf("formatConnectedIPs(IPv6-only) = %q, want \"IPv6=2409:8088::1234/64\"", v6Only)
	}

	dual := formatConnectedIPs(&qmi.RuntimeSettings{
		IPv4Address: net.ParseIP("10.1.2.3"),
		IPv6Address: net.ParseIP("2409:8088::1234"),
		IPv6Prefix:  64,
	})
	if dual != "IPv4=10.1.2.3, IPv6=2409:8088::1234/64" {
		t.Fatalf("formatConnectedIPs(dual-stack) = %q", dual)
	}
}

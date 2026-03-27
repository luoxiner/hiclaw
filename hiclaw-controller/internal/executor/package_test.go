package executor

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestValidateNacosURI_FormatErrors(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		wantErr string
	}{
		{
			name:    "wrong scheme",
			uri:     "http://host:8848/ns/spec",
			wantErr: "scheme must be nacos://",
		},
		{
			name:    "missing host",
			uri:     "nacos:///ns/spec",
			wantErr: "missing host",
		},
		{
			name:    "missing namespace and spec (no path)",
			uri:     "nacos://host:8848",
			wantErr: "expected nacos://",
		},
		{
			name:    "missing spec name (only namespace)",
			uri:     "nacos://host:8848/ns",
			wantErr: "expected nacos://",
		},
		{
			name:    "empty string",
			uri:     "",
			wantErr: "scheme must be nacos://",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNacosURI(context.Background(), tt.uri)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestValidateNacosURI_ValidFormat_UnreachableServer(t *testing.T) {
	// Valid format but server is unreachable — should pass format checks
	// and fail at the connection/preflight stage.
	tests := []struct {
		name string
		uri  string
	}{
		{
			name: "basic host:port",
			uri:  "nacos://127.0.0.1:19999/ns/my-spec",
		},
		{
			name: "with credentials",
			uri:  "nacos://admin:secret@127.0.0.1:19999/ns/my-spec",
		},
		{
			name: "with version",
			uri:  "nacos://127.0.0.1:19999/ns/my-spec/v1.0.0",
		},
		{
			name: "with label version",
			uri:  "nacos://admin:pass@127.0.0.1:19999/ns/my-spec/label:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNacosURI(context.Background(), tt.uri)
			if err == nil {
				t.Fatal("expected connection error for unreachable server, got nil")
			}
			// Should be a preflight/connection error, not a format error
			if strings.Contains(err.Error(), "scheme must be") ||
				strings.Contains(err.Error(), "missing host") ||
				strings.Contains(err.Error(), "expected nacos://[user:pass@]host:port") {
				t.Errorf("got format error instead of connection error: %v", err)
			}
			if !strings.Contains(err.Error(), "preflight check failed") {
				t.Errorf("expected preflight check error, got: %v", err)
			}
		})
	}
}

func TestResolveNacos_URIParsing(t *testing.T) {
	// Test that resolveNacos correctly extracts addr, namespace, specName,
	// and version from the URI. We can't reach a real server, but we can
	// verify the parsing by checking the error messages.
	tests := []struct {
		name    string
		uri     string
		wantErr string
	}{
		{
			name:    "too few path segments",
			uri:     "nacos://host:8848/only-namespace",
			wantErr: "invalid nacos URI",
		},
		{
			name:    "empty path",
			uri:     "nacos://host:8848",
			wantErr: "invalid nacos URI",
		},
	}

	resolver := NewPackageResolver(t.TempDir())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.uri)
			if err != nil {
				t.Fatalf("url.Parse failed: %v", err)
			}
			_, err = resolver.resolveNacos(context.Background(), u)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestResolveNacos_AddrExtraction(t *testing.T) {
	// Verify that the Nacos address is correctly built from the URI authority.
	// These will fail at connection time, but the error should reference the
	// correct address (not HICLAW_NACOS_ADDR).
	tests := []struct {
		name string
		uri  string
		// We just verify it doesn't mention HICLAW_NACOS_ADDR
	}{
		{
			name: "plain host",
			uri:  "nacos://10.0.0.1:8848/ns/spec",
		},
		{
			name: "host with credentials",
			uri:  "nacos://user:pass@10.0.0.1:8848/ns/spec",
		},
	}

	resolver := NewPackageResolver(t.TempDir())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.uri)
			if err != nil {
				t.Fatalf("url.Parse failed: %v", err)
			}
			_, err = resolver.resolveNacos(context.Background(), u)
			if err == nil {
				// Unreachable server, should error
				t.Fatal("expected error for unreachable server, got nil")
			}
			if strings.Contains(err.Error(), "HICLAW_NACOS_ADDR") {
				t.Errorf("error should not reference HICLAW_NACOS_ADDR, got: %v", err)
			}
		})
	}
}

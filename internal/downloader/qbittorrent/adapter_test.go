package qbittorrent

import "testing"

func TestEndpointPolicy(t *testing.T) {
	allowed := []string{"https://seedbox.example", "http://127.0.0.1:8080", "http://[::1]:8080"}
	for _, endpoint := range allowed {
		if _, err := New(endpoint); err != nil {
			t.Fatalf("%s should be allowed: %v", endpoint, err)
		}
	}
	blocked := []string{"http://192.168.1.2:8080", "http://seedbox.example", "https://user:pass@example.com", "ftp://example.com"}
	for _, endpoint := range blocked {
		if _, err := New(endpoint); err == nil {
			t.Fatalf("%s should be blocked", endpoint)
		}
	}
}

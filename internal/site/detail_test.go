package site

import "testing"

func TestTorrentDetailLimitsAndConfig(t *testing.T) {
	limits := DefaultTorrentDetailLimits()
	if limits.MaxRequests != 1 || limits.MaxResponseBytes != 4<<20 || limits.MaxResponseHeaderBytes != 64<<10 || limits.Validate() != nil {
		t.Fatalf("defaults=%#v", limits)
	}
	for _, invalid := range []TorrentDetailLimits{
		{},
		{MaxRequests: 2, MaxResponseBytes: 1, MaxResponseHeaderBytes: 1},
		{MaxRequests: 1, MaxResponseBytes: 8<<20 + 1, MaxResponseHeaderBytes: 1},
		{MaxRequests: 1, MaxResponseBytes: 1, MaxResponseHeaderBytes: 64<<10 + 1},
	} {
		if invalid.Validate() == nil {
			t.Fatalf("invalid limits accepted: %#v", invalid)
		}
	}
	config, err := NewTorrentDetailConfig("https://example.test", "example.details_by_id.v1")
	if err != nil || config.Validate() != nil {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	for _, invalid := range [][2]string{
		{"http://example.test", "example.details.v1"},
		{"https://user@example.test", "example.details.v1"},
		{"https://example.test/path", "example.details.v1"},
		{"https://example.test", "bad route"},
	} {
		if _, err := NewTorrentDetailConfig(invalid[0], invalid[1]); err == nil {
			t.Fatalf("invalid config accepted: %#v", invalid)
		}
	}
}

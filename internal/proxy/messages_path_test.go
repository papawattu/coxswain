package proxy

import "testing"

// TestIsMessagesPath pins which request paths get the Anthropic credential
// header (x-api-key) instead of Authorization (O2, D148; pi3's P3 on #531).
// A path that only shares the prefix ("/v1/messagesx") or puts the segment
// elsewhere ("/api/v1/messages") is not the Messages API.
func TestIsMessagesPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/v1/messages", true},
		{"/v1/messages/", true},
		{"/v1/messages/count_tokens", true},
		{"/v1/messages/batches/abc", true},
		{"/v1/messagesx", false},
		{"/v1/messages-legacy", false},
		{"/v1/message", false},
		{"/v1/chat/completions", false},
		{"/v1/responses", false},
		{"/v1/models", false},
		{"/messages", false},
		{"/api/v1/messages", false},
		{"/V1/messages", false},
		{"v1/messages", false},
		{"", false},
		{"/", false},
	} {
		if got := isMessagesPath(tc.path); got != tc.want {
			t.Errorf("isMessagesPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

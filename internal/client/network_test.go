package client

import "testing"

func TestNewAllowsPlainHTTPOnlyForLoopbackAndPrivateVPNAddresses(t *testing.T) {
	t.Parallel()
	for _, url := range []string{"http://127.0.0.1:7432", "http://100.100.1.2:7432", "http://10.8.0.1:7432", "https://erka.example"} {
		if _, err := New(url, "token", nil); err != nil {
			t.Fatalf("New(%q) error = %v", url, err)
		}
	}
	for _, url := range []string{"http://203.0.113.5:7432", "http://erka.example"} {
		if _, err := New(url, "token", nil); err == nil {
			t.Fatalf("New(%q) accepted plain HTTP on a public address", url)
		}
	}
}

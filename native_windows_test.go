//go:build windows && (amd64 || arm64) && !safem_test_noaudio

package moho

import "testing"

func TestNativeInventory(t *testing.T) {
	b := &wasapiBackend{}
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.ListDevices(); err != nil {
		t.Fatal(err)
	}
}

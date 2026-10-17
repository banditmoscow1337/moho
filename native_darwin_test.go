//go:build darwin && (amd64 || arm64) && !safem_test_noaudio

package audio

import "testing"

func TestNativeInventory(t *testing.T) {
	b := &coreAudioBackend{}
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, _, err := b.ListDevices(); err != nil {
		t.Fatal(err)
	}
}

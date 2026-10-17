//go:build windows && (amd64 || arm64)

package opuscodec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ebitengine/purego"
)

type library struct{ dll *syscall.DLL }

func openLibrary() (library, error) {
	path := os.Getenv("SAFEM_OPUS_LIBRARY")
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return library{}, fmt.Errorf("locate executable for libopus.dll: %w", err)
		}
		path = filepath.Join(filepath.Dir(exe), "libopus.dll")
	}
	if !filepath.IsAbs(path) {
		return library{}, errors.New("SAFEM_OPUS_LIBRARY must be an absolute path")
	}
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return library{}, fmt.Errorf("load libopus DLL from %q (use a DLL matching the process architecture): %w", path, err)
	}
	return library{dll}, nil
}

func (l library) bind(dst any, name string) error {
	proc, err := l.dll.FindProc(name)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", name, err)
	}
	purego.RegisterFunc(dst, proc.Addr())
	return nil
}

func (l library) close() error { return l.dll.Release() }

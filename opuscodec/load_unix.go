//go:build (darwin || linux) && (amd64 || arm64)

package opuscodec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ebitengine/purego"
)

type library struct{ handle uintptr }

func openLibrary() (library, error) {
	if path := os.Getenv("SAFEM_OPUS_LIBRARY"); path != "" {
		if !filepath.IsAbs(path) {
			return library{}, errors.New("SAFEM_OPUS_LIBRARY must be an absolute path")
		}
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			return library{}, fmt.Errorf("load libopus from %q: %w", path, err)
		}
		return library{h}, nil
	}
	paths := []string{"libopus.so.0"}
	if runtime.GOOS == "darwin" {
		paths = []string{
			"/opt/homebrew/opt/opus/lib/libopus.0.dylib",
			"/usr/local/opt/opus/lib/libopus.0.dylib",
			"libopus.0.dylib", "libopus.dylib",
		}
	}
	var errs []error
	for _, path := range paths {
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err == nil {
			return library{h}, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}
	return library{}, fmt.Errorf("load libopus (install its runtime library or set SAFEM_OPUS_LIBRARY): %w", errors.Join(errs...))
}

func (l library) bind(dst any, name string) error {
	addr, err := purego.Dlsym(l.handle, name)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", name, err)
	}
	purego.RegisterFunc(dst, addr)
	return nil
}

func (l library) close() error { return purego.Dlclose(l.handle) }

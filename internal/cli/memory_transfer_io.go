package cli

import (
	"fmt"
	"io"
	"os"
)

// writeFileExcl writes data to a new file, refusing to overwrite an existing one.
func writeFileExcl(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readFileBounded reads a tarball, refusing anything over 16 MiB.
func readFileBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxTarballBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTarballBytes {
		return nil, fmt.Errorf("%s is larger than 16 MiB", path)
	}
	return data, nil
}

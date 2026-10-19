//go:build unix

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openWorkspaceRoot(path string) (*os.File, error) {
	rel, err := absoluteComponents(path)
	if err != nil {
		return nil, err
	}
	root, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return openRelative(root, rel, unix.O_RDONLY|unix.O_DIRECTORY, false)
}

func openWorkspaceFile(root *os.File, rel string, write, createParents bool) (*os.File, error) {
	flags := unix.O_RDONLY
	if write {
		flags = unix.O_WRONLY | unix.O_CREAT
	}
	f, err := openRelative(root, rel, flags|unix.O_NONBLOCK, createParents)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() && (!info.IsDir() || write) {
		err = fmt.Errorf("file tool does not support special file: %s", f.Name())
	}
	if err == nil && write {
		var stat unix.Stat_t
		err = unix.Fstat(int(f.Fd()), &stat)
		if err == nil && stat.Nlink > 1 {
			err = fmt.Errorf("writing hard-linked files is denied: %s", f.Name())
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func openRelative(root *os.File, rel string, flags int, createParents bool) (*os.File, error) {
	if rel == "" {
		rel = "."
	}
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("invalid scoped path: %q", rel)
	}
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	dir := root
	for _, part := range parts[:len(parts)-1] {
		fd, err := unix.Openat(int(dir.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.ENOENT && createParents {
			if mkdirErr := unix.Mkdirat(int(dir.Fd()), part, 0o755); mkdirErr != nil && mkdirErr != unix.EEXIST {
				if dir != root {
					dir.Close()
				}
				return nil, mkdirErr
			}
			fd, err = unix.Openat(int(dir.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if dir != root {
			dir.Close()
		}
		if err != nil {
			return nil, err
		}
		dir = os.NewFile(uintptr(fd), part)
	}
	if dir != root {
		defer dir.Close()
	}
	fd, err := unix.Openat(int(dir.Fd()), parts[len(parts)-1], flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(root.Name(), rel)), nil
}

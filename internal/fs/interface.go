package fs

import (
	"io"
	"os"
)

// FS bundles all methods needed for a file system.
type FS interface {
	Open(name string) (File, error)
	OpenFile(name string, flag int, perm os.FileMode) (File, error)
	Lstat(name string) (os.FileInfo, error)
}

// File is an open file on a file system.
type File interface {
	io.Reader
	io.Writer
	io.Closer

	Fd() uintptr
	Readdirnames(n int) ([]string, error)
	Readdir(int) ([]os.FileInfo, error)
	Seek(int64, int) (int64, error)
	Stat() (os.FileInfo, error)
	Name() string
}
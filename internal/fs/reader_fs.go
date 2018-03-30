package fs

import (
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/restic/restic/internal/errors"
)

// Reader is a file system which provides a directory with a single file. When
// this file is opened for reading, the reader is passed through. The file can
// be opened once, all subsequent open calls return syscall.EIO. For Lstat(),
// the provided FileInfo is returned.
type Reader struct {
	Name string
	io.ReadCloser
	os.FileInfo

	open sync.Once
}

// statically ensure that Local implements FS.
var _ FS = &Reader{}

// Open opens a file for reading.
func (fs *Reader) Open(name string) (f File, err error) {
	switch name {
	case fs.Name:
		fs.open.Do(func() {
			f = readerFile{ReadCloser: fs.ReadCloser}
		})

		if f == nil {
			return nil, syscall.EIO
		}

		return f, nil
	case "/":
		f = fakeDir{
			entries: []os.FileInfo{fs.FileInfo},
		}
		return f, nil
	}

	return nil, syscall.ENOENT
}

// OpenFile is the generalized open call; most users will use Open
// or Create instead.  It opens the named file with specified flag
// (O_RDONLY etc.) and perm, (0666 etc.) if applicable.  If successful,
// methods on the returned File can be used for I/O.
// If there is an error, it will be of type *PathError.
func (fs *Reader) OpenFile(name string, flag int, perm os.FileMode) (f File, err error) {
	if flag != os.O_RDONLY {
		return nil, syscall.EIO
	}

	fs.open.Do(func() {
		f = readerFile{ReadCloser: fs.ReadCloser}
	})

	if f == nil {
		return nil, syscall.EIO
	}

	return f, nil
}

// Lstat returns the FileInfo structure describing the named file.
// If the file is a symbolic link, the returned FileInfo
// describes the symbolic link.  Lstat makes no attempt to follow the link.
// If there is an error, it will be of type *PathError.
func (fs *Reader) Lstat(name string) (os.FileInfo, error) {
	return os.Lstat(fixpath(name))
}

type readerFile struct {
	io.ReadCloser
	fakeFile
}

func (r readerFile) Read(p []byte) (int, error) {
	return r.ReadCloser.Read(p)
}

func (r readerFile) Close() error {
	return r.ReadCloser.Close()
}

// ensure that readerFile implements File
var _ File = readerFile{}

// fakeFile implements all File methods, but only returns errors for anything
// except Stat() and Name().
type fakeFile struct {
	name string
	os.FileInfo
}

// ensure that fakeFile implements File
var _ File = fakeFile{}

func (f fakeFile) Fd() uintptr {
	return 0
}

func (f fakeFile) Readdirnames(n int) ([]string, error) {
	return nil, os.ErrInvalid
}

func (f fakeFile) Readdir(n int) ([]os.FileInfo, error) {
	return nil, os.ErrInvalid
}

func (f fakeFile) Seek(int64, int) (int64, error) {
	return 0, os.ErrInvalid
}

func (f fakeFile) Write(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f fakeFile) Read(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f fakeFile) Close() error {
	return os.ErrInvalid
}

func (f fakeFile) Stat() (os.FileInfo, error) {
	return f.FileInfo, nil
}

func (f fakeFile) Name() string {
	return f.name
}

// fakeDir implements Readdirnames and Readdir, everything else is delegated to fakeFile.
type fakeDir struct {
	entries []os.FileInfo
	fakeFile
}

func (d fakeDir) Readdirnames(n int) ([]string, error) {
	if n >= 0 {
		return nil, errors.New("not implemented")
	}
	names := make([]string, 0, len(d.entries))
	for _, entry := range d.entries {
		names = append(names, entry.Name())
	}

	return names, nil
}

func (d fakeDir) Readdir(n int) ([]os.FileInfo, error) {
	if n >= 0 {
		return nil, errors.New("not implemented")
	}
	return d.entries, nil
}

package fs

import (
	"os"
)

// Local is the local file system. Most methods are just passed on to the stdlib.
type Local struct{}

// statically ensure that Local implements FS.
var _ FS = &Local{}

// Open opens a file for reading.
func (fs Local) Open(name string) (File, error) {
	return os.Open(fixpath(name))
}

// OpenFile is the generalized open call; most users will use Open
// or Create instead.  It opens the named file with specified flag
// (O_RDONLY etc.) and perm, (0666 etc.) if applicable.  If successful,
// methods on the returned File can be used for I/O.
// If there is an error, it will be of type *PathError.
func (fs Local) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	return os.OpenFile(fixpath(name), flag, perm)
}

// Lstat returns the FileInfo structure describing the named file.
// If the file is a symbolic link, the returned FileInfo
// describes the symbolic link.  Lstat makes no attempt to follow the link.
// If there is an error, it will be of type *PathError.
func (fs Local) Lstat(name string) (os.FileInfo, error) {
	return os.Lstat(fixpath(name))
}

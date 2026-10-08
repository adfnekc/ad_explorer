package rdp

import (
	"io"
	"time"

	"github.com/adfnekc/grdp/plugin/cliprdr"
)

// FileProvider is the application's view of the files it shares with the remote.
//
// The protocol layer never opens a path itself: it asks for the manifest and
// then for ranges of bytes. That is what lets the gateway serve files it never
// stores, and it is why a remote end cannot name a path on this machine.
type FileProvider interface {
	// Names returns the names to publish, in the order they should appear.
	Names() []string
	// Info reports the size, modification time and directory flag of one entry.
	Info(name string) (size int64, modTime time.Time, isDir bool)
	// Open returns the contents of one entry.
	//
	// The returned reader is asked to seek to an offset and then read a range,
	// so a reader that implements io.Seeker is served directly and one that
	// does not is read through and discarded. A gateway wants the first.
	Open(name string) (io.ReadCloser, error)
}

// RemoteFile is one entry the remote put on the clipboard.
type RemoteFile struct {
	// Index is the position in the manifest the remote sent.
	Index uint32
	Name  string
	Size  int64
	// IsDir reports a directory. Recursing into it is the application's
	// decision; the manifest does not flatten it.
	IsDir bool
}

// Files is the file transfer surface of a session.
//
// A file transfer is not clipboard text: the protocol hands over a manifest and
// then asks for ranges of bytes, in both directions. Nothing file shaped is
// advertised until a provider is set, because a format the client cannot produce
// data for leaves the pasting side waiting.
type Files interface {
	// Offer publishes files the remote may paste. Passing nil withdraws them.
	Offer(p FileProvider)
	// OnRemoteFiles registers f to be called with the files the remote offers.
	OnRemoteFiles(f func([]RemoteFile)) error
	// ReadRemoteFile reads a range of a file the remote offered. The library
	// caps a single read, so a large file is read in ranges.
	ReadRemoteFile(index uint32, off int64, n int) ([]byte, error)
	// ClearRemoteFiles drops the manifest the remote sent.
	ClearRemoteFiles()
}

// Files returns the file transfer surface, or nil when the session has none.
//
// It is nil for anything but RDP, and for a session without the clipboard
// channel, which is the honest answer rather than a surface that silently does
// nothing.
func (s *Session) Files() Files {
	if s.cli == nil {
		return nil
	}
	c := s.cli.Files()
	if c == nil {
		return nil
	}
	return grdpFiles{c}
}

// grdpFiles adapts the library's file surface.
//
// The two interfaces are structurally identical, so the provider passes through
// without a wrapper and there is one implementation of reading ranges rather
// than two.
type grdpFiles struct{ c *cliprdr.CliprdrClient }

func (f grdpFiles) Offer(p FileProvider) { f.c.SetFileProvider(p) }

func (f grdpFiles) OnRemoteFiles(cb func([]RemoteFile)) error {
	f.c.OnRemoteFiles(func(files []cliprdr.RemoteFile) {
		out := make([]RemoteFile, 0, len(files))
		for _, rf := range files {
			out = append(out, RemoteFile{
				Index: rf.Index,
				Name:  rf.Name,
				Size:  rf.Size,
				IsDir: rf.IsDir,
			})
		}
		cb(out)
	})
	return nil
}

func (f grdpFiles) ReadRemoteFile(index uint32, off int64, n int) ([]byte, error) {
	return f.c.ReadRemoteFile(index, off, n)
}

func (f grdpFiles) ClearRemoteFiles() { f.c.ClearRemoteFiles() }

package session

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/adfnekc/ad_explorer/internal/rdp"
	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// File transfer timings and limits.
const (
	// fileRangeTimeout is how long a range request waits for the browser. It is
	// generous because the browser may be reading a large file from disk, and
	// short enough that a tab which has gone away does not hold a paste open.
	fileRangeTimeout = 20 * time.Second
	// maxRangePerRequest is how much of a file is asked for in one round trip.
	// Read-ahead of this size means roughly one round trip per megabyte rather
	// than one per read.
	maxRangePerRequest = 1 << 20
	// downloadChunk is how much of a remote file is sent to the browser in one
	// message. Small enough to keep a message well inside the frame limit and
	// to make progress visible.
	downloadChunk = 1 << 20
	// maxConcurrentDownloads bounds how many files a client may pull at once,
	// so a misbehaving one cannot open an unbounded number of remote reads.
	maxConcurrentDownloads = 4
)

// fileTransfer holds both directions of a session's file transfer.
//
// Neither direction stores anything: bytes the client offers are asked for as
// the remote requests them, and bytes the remote offers are streamed straight
// to the client. That is what keeps a multi-gigabyte file from having to fit in
// memory, and it keeps the gateway as stateless about file contents as it is
// about credentials.
type fileTransfer struct {
	files rdp.Files

	// emit and emitFrames talk to the client. They are installed by the
	// session, so the transfer never has to know how the client is reached.
	emit       func(wsframe.Control) error
	emitFrames func([]wsframe.Frame) error

	mu           sync.Mutex
	offered      []wsframe.OfferedFile
	manifest     []rdp.RemoteFile
	pendingRange map[uint32]chan []byte
	nextToken    uint32
	downloads    map[uint32]struct{}
	nextTransfer uint32
}

func newFileTransfer(files rdp.Files, emit func(wsframe.Control) error, emitFrames func([]wsframe.Frame) error) *fileTransfer {
	return &fileTransfer{
		files:        files,
		emit:         emit,
		emitFrames:   emitFrames,
		pendingRange: make(map[uint32]chan []byte),
		downloads:    make(map[uint32]struct{}),
	}
}

// ---------------------------------------------------------------- wiring

// startFiles connects the session's file transfer to the remote, when the
// transport has one.
func (s *Session) startFiles() {
	files := s.remote.Files()
	if files == nil {
		// Nothing is said: a transport without file transfer is a transport
		// without file transfer, and inventing a complaint for every session
		// would train an operator to ignore the notices.
		return
	}
	ft := newFileTransfer(files, s.send, s.sendFrames)
	if err := files.OnRemoteFiles(ft.onRemoteFiles); err != nil {
		s.reportUnsupported("files", "file transfer is unavailable: "+err.Error())
		return
	}

	s.mu.Lock()
	s.files = ft
	s.mu.Unlock()
}

// onRemoteFiles forwards the remote's clipboard manifest.
//
// The manifest is kept as well as forwarded: a download needs the size of the
// entry it was asked for, and the remote does not repeat it in the reply.
func (ft *fileTransfer) onRemoteFiles(files []rdp.RemoteFile) {
	ft.mu.Lock()
	ft.manifest = append([]rdp.RemoteFile(nil), files...)
	ft.mu.Unlock()

	out := make([]wsframe.RemoteFile, 0, len(files))
	for _, f := range files {
		out = append(out, wsframe.RemoteFile{
			Index: f.Index, Name: f.Name, Size: f.Size, IsDir: f.IsDir,
		})
	}
	_ = ft.emit(wsframe.FilesRemote{Files: out})
}

// ---------------------------------------------------------------- client side

// handleFileControl applies one file transfer message from the client. It
// reports whether the message was a file transfer message at all.
func (s *Session) handleFileControl(c wsframe.Control) bool {
	s.mu.Lock()
	ft := s.files
	s.mu.Unlock()

	switch m := c.(type) {
	case *wsframe.FilesOffer:
		if ft == nil {
			s.reportUnsupported("files", "file transfer is unavailable on this session")
			return true
		}
		ft.offer(m.Files)
		return true

	case *wsframe.FilesClear:
		if ft == nil {
			return true
		}
		ft.clearOffer()
		return true

	case *wsframe.FilesGet:
		if ft == nil {
			s.reportUnsupported("files", "file transfer is unavailable on this session")
			return true
		}
		ft.download(m.Index)
		return true

	case *wsframe.FilesCancel:
		if ft == nil {
			return true
		}
		// A cancelled transfer is noticed by its sender: the frames stop and
		// the session ends it. Nothing to undo here.
		return true

	default:
		return false
	}
}

// handleFileChunk answers a range the gateway asked the client for.
func (s *Session) handleFileChunk(token, offset uint32, data []byte) {
	s.mu.Lock()
	ft := s.files
	s.mu.Unlock()
	if ft == nil {
		return
	}
	ft.deliver(token, data)
}

// ---------------------------------------------------------------- offering

// offer installs a provider that serves the files the client listed.
func (ft *fileTransfer) offer(files []wsframe.OfferedFile) {
	ft.mu.Lock()
	ft.offered = append([]wsframe.OfferedFile(nil), files...)
	provider := &browserProvider{ft: ft, files: ft.offered}
	ft.mu.Unlock()

	// Nothing file shaped is advertised until this is set: a format the client
	// cannot produce data for leaves the pasting side waiting.
	ft.files.Offer(provider)
}

// clearOffer withdraws the files, so the remote stops being able to paste them.
func (ft *fileTransfer) clearOffer() {
	ft.mu.Lock()
	ft.offered = nil
	ft.mu.Unlock()
	ft.files.Offer(nil)
}

// browserProvider is the FileProvider the remote's paste is served from.
type browserProvider struct {
	ft    *fileTransfer
	files []wsframe.OfferedFile
}

func (p *browserProvider) Names() []string {
	out := make([]string, 0, len(p.files))
	for _, f := range p.files {
		out = append(out, f.Name)
	}
	return out
}

func (p *browserProvider) Info(name string) (int64, time.Time, bool) {
	for _, f := range p.files {
		if f.Name == name {
			// The browser does not report a modification time, and inventing
			// one would put a wrong date in the remote's paste dialog.
			return f.Size, time.Time{}, false
		}
	}
	return 0, time.Time{}, false
}

func (p *browserProvider) Open(name string) (io.ReadCloser, error) {
	for _, f := range p.files {
		if f.Name == name {
			return &browserFile{ft: p.ft, name: name, size: f.Size}, nil
		}
	}
	return nil, fmt.Errorf("session: %q is not one of the offered files", name)
}

// browserFile reads a file the client offered by asking the client for it.
//
// The protocol layer seeks and then reads a range, so this implements both, and
// reads ahead by a megabyte: without that, io.ReadAll's growing buffer would
// turn one range into a round trip every few hundred bytes.
type browserFile struct {
	ft   *fileTransfer
	name string
	size int64
	off  int64

	buf    []byte
	bufOff int64
}

func (f *browserFile) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.size > 0 && f.off >= f.size {
		return 0, io.EOF
	}

	if len(f.buf) == 0 {
		want := maxRangePerRequest
		if f.size > 0 && f.size-f.off < int64(want) {
			want = int(f.size - f.off)
		}
		if want <= 0 {
			return 0, io.EOF
		}
		data, err := f.ft.fetchRange(f.name, f.off, want)
		if err != nil {
			return 0, err
		}
		if len(data) == 0 {
			return 0, io.EOF
		}
		f.buf, f.bufOff = data, f.off
	}

	n := copy(p, f.buf[f.off-f.bufOff:])
	f.off += int64(n)
	if f.off-f.bufOff >= int64(len(f.buf)) {
		f.buf = nil
	}
	return n, nil
}

func (f *browserFile) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = f.off + off
	case io.SeekEnd:
		abs = f.size + off
	default:
		return 0, fmt.Errorf("session: unknown whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("session: cannot seek before the start of a file")
	}
	f.off = abs
	// The read-ahead no longer covers where we are, and keeping it would serve
	// the wrong bytes.
	f.buf = nil
	return abs, nil
}

func (f *browserFile) Close() error {
	f.buf = nil
	return nil
}

// fetchRange asks the client for a range of an offered file and waits for it.
func (ft *fileTransfer) fetchRange(name string, off int64, length int) ([]byte, error) {
	ft.mu.Lock()
	ft.nextToken++
	token := ft.nextToken
	ch := make(chan []byte, 1)
	ft.pendingRange[token] = ch
	ft.mu.Unlock()

	defer func() {
		ft.mu.Lock()
		delete(ft.pendingRange, token)
		ft.mu.Unlock()
	}()

	if err := ft.emit(wsframe.FilesNeed{Token: token, Name: name, Offset: off, Length: length}); err != nil {
		return nil, err
	}

	select {
	case data := <-ch:
		return data, nil
	case <-time.After(fileRangeTimeout):
		return nil, fmt.Errorf("session: the client did not answer a request for %d bytes of %q at %d within %s",
			length, name, off, fileRangeTimeout)
	}
}

// deliver hands a range to whoever asked for it.
func (ft *fileTransfer) deliver(token uint32, data []byte) {
	ft.mu.Lock()
	ch := ft.pendingRange[token]
	ft.mu.Unlock()
	if ch == nil {
		// A range that nobody is waiting for: the request timed out or the
		// download was cancelled. Dropping it is right; the alternative is a
		// goroutine held on to forever.
		return
	}
	select {
	case ch <- data:
	default:
	}
}

// ---------------------------------------------------------------- downloading

// download streams a file the remote offered to the client.
//
// The library's read blocks for up to its own timeout, so it runs on its own
// goroutine rather than on the session's event loop.
func (ft *fileTransfer) download(index uint32) {
	ft.mu.Lock()
	if len(ft.downloads) >= maxConcurrentDownloads {
		ft.mu.Unlock()
		_ = ft.emit(wsframe.ErrorMsgFrom(wsframe.ErrCodeUnsupported,
			"too many files are being downloaded at once"))
		return
	}
	ft.nextTransfer++
	transferID := ft.nextTransfer
	ft.downloads[transferID] = struct{}{}
	ft.mu.Unlock()

	go func() {
		defer func() {
			ft.mu.Lock()
			delete(ft.downloads, transferID)
			ft.mu.Unlock()
		}()
		ft.stream(index, transferID)
	}()
}

// stream reads a remote file in chunks and sends them. It ends with a FilesEnd
// carrying the error, if any, so the client always learns how it went.
func (ft *fileTransfer) stream(index uint32, transferID uint32) {
	manifest := ft.remoteManifest()
	entry, ok := entryAt(manifest, index)
	if !ok {
		_ = ft.emit(wsframe.FilesEnd{TransferID: transferID,
			Error: fmt.Sprintf("the remote has no file at index %d", index)})
		return
	}
	if entry.IsDir {
		_ = ft.emit(wsframe.FilesEnd{TransferID: transferID,
			Error: fmt.Sprintf("%q is a directory", entry.Name)})
		return
	}

	_ = ft.emit(wsframe.FilesBegin{
		TransferID: transferID, Index: index, Name: entry.Name, Size: entry.Size,
	})

	var sent int64
	for entry.Size == 0 || sent < entry.Size {
		want := downloadChunk
		if entry.Size > 0 && entry.Size-sent < int64(want) {
			want = int(entry.Size - sent)
		}
		chunk, err := ft.files.ReadRemoteFile(index, sent, want)
		if err != nil {
			_ = ft.emit(wsframe.FilesEnd{TransferID: transferID, Error: err.Error()})
			return
		}
		if len(chunk) == 0 {
			break // the file ended earlier than its manifest said
		}

		frame, err := wsframe.FileChunkFrame(transferID, uint32(sent), chunk)
		if err != nil {
			_ = ft.emit(wsframe.FilesEnd{TransferID: transferID, Error: err.Error()})
			return
		}
		if err := ft.emitFrames([]wsframe.Frame{frame}); err != nil {
			// The client is gone; the session will notice on its own.
			return
		}
		sent += int64(len(chunk))
	}

	_ = ft.emit(wsframe.FilesEnd{TransferID: transferID})
}

// remoteManifest is the last list the remote sent, used to size a download.
func (ft *fileTransfer) remoteManifest() []rdp.RemoteFile {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]rdp.RemoteFile(nil), ft.manifest...)
}

func entryAt(files []rdp.RemoteFile, index uint32) (rdp.RemoteFile, bool) {
	for _, f := range files {
		if f.Index == index {
			return f, true
		}
	}
	return rdp.RemoteFile{}, false
}

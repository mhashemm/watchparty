package party

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// torrentStore keeps a torrent in one sparse file, read back only through the client. The
// library's own file store memory-maps and never unmaps, so Windows can't delete a partly
// downloaded file and a full disk crashes the process instead of failing a write.
type torrentStore struct{ dir string }

func (s torrentStore) OpenTorrent(_ context.Context, info *metainfo.Info, _ metainfo.Hash) (storage.TorrentImpl, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, "data"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return storage.TorrentImpl{}, err
	}
	sparse(f)
	if err = f.Truncate(info.TotalLength()); err != nil {
		f.Close()
		return storage.TorrentImpl{}, err
	}
	complete := make([]atomic.Bool, info.NumPieces())
	return storage.TorrentImpl{
		Piece: func(p metainfo.Piece) storage.PieceImpl {
			return torrentPiece{f, p.Offset(), &complete[p.Index()]}
		},
		Close: f.Close,
	}, nil
}

type torrentPiece struct {
	f        *os.File
	offset   int64
	complete *atomic.Bool
}

func (p torrentPiece) ReadAt(b []byte, off int64) (int, error) {
	return p.f.ReadAt(b, p.offset+off)
}

func (p torrentPiece) WriteAt(b []byte, off int64) (int, error) {
	return p.f.WriteAt(b, p.offset+off)
}

func (p torrentPiece) MarkComplete() error {
	p.complete.Store(true)
	return nil
}

func (p torrentPiece) MarkNotComplete() error {
	p.complete.Store(false)
	return nil
}

func (p torrentPiece) Completion() storage.Completion {
	return storage.Completion{Ok: true, Complete: p.complete.Load()}
}

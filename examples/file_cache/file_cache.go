package filecache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/richardartoul/nola/virtual"
	"github.com/richardartoul/nola/virtual/registry"
	"github.com/richardartoul/nola/wapcutils"
)

type Fetcher interface {
	FetchRange(ctx context.Context, offset, length int) (io.ReadCloser, error)
}

type ChunkCache interface {
	Get(b []byte, chunkIdx int) ([]byte, bool, error)
	Put(chunkIdx int, b []byte) error
}

type FileCacheModule struct {
	chunkSize int
	fetchSize int

	fetcher    Fetcher
	chunkCache ChunkCache
}

func NewFileCacheModule(
	chunkSize int,
	fetchSize int,
	fetcher Fetcher,
	chunkCache ChunkCache,
) *FileCacheModule {
	return &FileCacheModule{
		chunkSize: chunkSize,
		fetchSize: fetchSize,

		fetcher:    fetcher,
		chunkCache: chunkCache,
	}
}

type FileCacheInstantiatePayload struct {
	FileSize int
}

func (f *FileCacheModule) Instantiate(
	ctx context.Context,
	id string,
	payload []byte,
	host virtual.HostCapabilities,
) (virtual.Actor, error) {
	p := &FileCacheInstantiatePayload{}
	if err := json.Unmarshal(payload, p); err != nil {
		return nil, fmt.Errorf("error unmarshaling FileCacheInstantiatePayload: %w", err)
	}
	if p.FileSize <= 0 {
		return nil, fmt.Errorf("filesize cannot be <= 0, but was: %d", p.FileSize)
	}

	return NewFileCacheActor(p.FileSize, f.chunkSize, f.fetchSize, f.fetcher, f.chunkCache)
}

func (f *FileCacheModule) Close(ctx context.Context) error {
	return nil
}

type FileCacheActor struct {
	sync.Mutex

	// Constants.
	fileSize  int
	chunkSize int
	fetchSize int

	// State.
	bufPool *sync.Pool

	// Dependencies.
	fetcher    Fetcher
	chunkCache ChunkCache
}

func NewFileCacheActor(
	fileSize int,
	chunkSize int,
	fetchSize int,

	fetcher Fetcher,
	chunkCache ChunkCache,
) (virtual.ActorStream, error) {
	if fetchSize%chunkSize != 0 || chunkSize > fetchSize {
		return nil, fmt.Errorf("%d does not cleanly divide %d", fetchSize, chunkSize)
	}

	return &FileCacheActor{
		fileSize:  fileSize,
		chunkSize: chunkSize,
		fetchSize: fetchSize,

		bufPool: &sync.Pool{
			New: func() any {
				return make([]byte, 0, chunkSize)
			},
		},

		fetcher:    fetcher,
		chunkCache: chunkCache,
	}, nil
}

type GetRangeRequest struct {
	StartOffset int `json:"start_offset"`
	EndOffset   int `json:"end_offset"`
}

func (f *FileCacheActor) InvokeStream(
	ctx context.Context,
	operation string,
	payload []byte,
	transaction registry.ActorKVTransaction,
) (io.ReadCloser, error) {
	switch operation {
	case wapcutils.StartupOperationName, wapcutils.ShutdownOperationName:
		return nil, nil
	case "getRange":
		req := &GetRangeRequest{}
		if err := json.Unmarshal(payload, req); err != nil {
			return nil, fmt.Errorf("error unmarshaling GetRangeRequest: %w", err)
		}

		if req.StartOffset < 0 ||
			req.EndOffset < 0 ||
			req.EndOffset <= req.StartOffset ||
			req.EndOffset > f.fileSize {
			return nil, fmt.Errorf("invalid GetRangeRequest: %+v, f.fileSize: %d", req, f.fileSize)
		}

		reader, writer := io.Pipe()
		go func() {
			f.getRange(ctx, writer, req.StartOffset, req.EndOffset)
		}()
		return reader, nil
	default:
		return nil, fmt.Errorf("unhandled operation: %s", operation)
	}
}

func (f *FileCacheActor) Close(ctx context.Context) error {
	return nil
}

func (f *FileCacheActor) getRange(
	ctx context.Context,
	w *io.PipeWriter,
	start,
	end int,
) {
	chunksToRead := f.rangeToChunkIndexes(start, end)
	for _, chunk := range chunksToRead {
		if err := f.copyChunk(ctx, w, chunk); err != nil {
			w.CloseWithError(err)
			return
		}
	}
	w.Close()
}

func (f *FileCacheActor) copyChunk(
	ctx context.Context,
	w io.Writer,
	toRead chunkToRead,
) error {
	// TODO: Do this in caller so we can reuse buffer across many calls to
	//       copyChunk to avoid going back and forth to the pool.
	bufI := f.bufPool.Get()
	defer f.bufPool.Put(bufI)
	buf := bufI.([]byte)[:0]

	// First try and copy the requested chunk out from the cache.
	chunk, ok, err := f.chunkCache.Get(buf[:0], toRead.idx)
	if err != nil {
		return fmt.Errorf("error copying chunk from cache: %w", err)
	}
	if ok {
		// Chunk was in the cache so we just need to copy it over and we're done.
		_, err := w.Write(chunk[toRead.start:toRead.end])
		if err != nil {
			return fmt.Errorf("error writing chunk to output writer: %w", err)
		}
		return nil
	}

	// Chunk was not in cache, we need to fetch.
	// TODO: deduplicate fetches.
	start, end := f.chunkIndexToFetchRange(toRead.idx)
	r, err := f.fetcher.FetchRange(ctx, start, end)
	if err != nil {
		return fmt.Errorf(
			"error fetching range: [%d:%d] for chunk idx: %d, err: %w",
			start, end, toRead.idx, err)
	}

	var (
		remaining = end - start
		chunkIdx  = f.offsetToChunkIndex(start)
	)
	for i := 0; remaining > 0; i++ {
		toCopy := remaining
		if toCopy > f.chunkSize {
			toCopy = f.chunkSize
		}
		buf := bytes.NewBuffer(buf[:0])
		n, err := io.CopyN(buf, r, int64(toCopy))
		if err != nil {
			return fmt.Errorf("error copying from fetch: %w", err)
		}
		if n != int64(toCopy) {
			return fmt.Errorf(
				"expected to copy: %d bytes but copied: %d",
				toCopy, n)
		}

		err = f.chunkCache.Put(chunkIdx, buf.Bytes())
		if err != nil {
			return fmt.Errorf("error storing chunk: %d in cache: %w", chunkIdx, err)
		}
		remaining -= toCopy
		chunkIdx++
	}

	chunk, ok, err = f.chunkCache.Get(buf[:0], toRead.idx)
	if err != nil {
		return fmt.Errorf("error copying chunk from cache: %w", err)
	}
	if ok {
		// Chunk was in the cache so it was copied, we're done.
		_, err := w.Write(chunk[toRead.start:toRead.end])
		if err != nil {
			return fmt.Errorf("error writing chunk to output writer: %w", err)
		}
		return nil
	}

	return fmt.Errorf("chunk: %d was not in cache after fetch", toRead.idx)
}

type chunkToRead struct {
	idx   int
	start int
	end   int
}

// TODO: Refactor/clean this function.
func (f *FileCacheActor) rangeToChunkIndexes(start, end int) []chunkToRead {
	var chunkIndexes []chunkToRead

	remaining := end - start
	curr := start
	for i := 0; remaining > 0; i++ {
		if i == 0 {
			chunkIdx := curr / f.chunkSize
			chunkStartOffset := curr % f.chunkSize
			chunkEndOffset := f.chunkSize
			if chunkStartOffset+remaining < f.chunkSize {
				chunkEndOffset = chunkStartOffset + remaining
			}
			chunkIndexes = append(chunkIndexes, chunkToRead{
				idx:   chunkIdx,
				start: chunkStartOffset,
				end:   chunkEndOffset,
			})
			remaining -= chunkEndOffset - chunkStartOffset
			curr += f.chunkSize
			continue
		}

		chunkIndexes = append(chunkIndexes, chunkToRead{
			idx:   curr / f.chunkSize,
			start: 0,
			end:   f.chunkSize,
		})
		remaining -= f.chunkSize
		curr += f.chunkSize
	}
	last := chunkIndexes[len(chunkIndexes)-1]
	if f.chunkSize*(last.idx+1) > end {
		chunkIndexes[len(chunkIndexes)-1].end = end - last.idx*f.chunkSize
	}

	return chunkIndexes
}

func (f *FileCacheActor) offsetToChunkIndex(offset int) int {
	return offset / f.chunkSize
}

func (f *FileCacheActor) chunkIndexToFetchRange(idx int) (int, int) {
	var (
		start = ((idx * f.chunkSize) / f.fetchSize) * f.fetchSize
		end   = start + f.fetchSize
	)
	if end > f.fileSize {
		end = f.fileSize
	}
	return start, end
}

package managedproxy

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"sync"
)

// parsedConfig caches one immutable decode. A cache hit still reads the
// entire regular file so in-place edits, including same-size edits with a
// preserved timestamp, are visible on the next check. Hashing to a stream
// avoids allocating a file-sized buffer on every steady-state poll.
type parsedConfig[T any] struct {
	mu     sync.Mutex
	path   string
	size   int64
	digest [sha256.Size]byte
	value  T
	err    error
}

func (c *parsedConfig[T]) read(path string, limit int64, decode func([]byte) (T, error)) (T, error) {
	var zero T
	f, err := os.Open(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.path == path {
			c.path, c.value, c.err = "", zero, nil
		}
		return zero, err
	}
	defer f.Close()
	if c.path == path {
		info, err := f.Stat()
		if err == nil && info.Mode().IsRegular() && info.Size() == c.size {
			h := sha256.New()
			if _, err := io.Copy(h, io.LimitReader(f, limit)); err != nil {
				return zero, err
			}
			if bytes.Equal(h.Sum(nil), c.digest[:]) {
				return c.value, c.err
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return zero, err
			}
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return zero, err
	}
	// An in-place writer can run between the comparison and the reread.
	// Key the result by the exact bytes supplied to the decoder.
	c.value, c.err = decode(data)
	c.path, c.size, c.digest = path, int64(len(data)), sha256.Sum256(data)
	return c.value, c.err
}

package managedproxy

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"sync"
)

// Applied is the single source of truth for the private endpoint used by Cage.
func Applied(profile, endpoint, revision string) (Store, error) {
	b, missing, err := readStore(profile)
	if err != nil {
		return Store{}, err
	}
	p, err := appliedStore(b, missing, endpoint, revision)
	if err != nil {
		return p, err
	}
	return p, retiredRouteCheck(profile)
}

// A previous core may still expose a retired account route even when the
// selected environment route is valid. Revocation also covers that interval.
// The core configuration is outside the store bytes, so this is never cached.
func retiredRouteCheck(profile string) error {
	if _, err := BoundInterface(filepath.Join(Dir(profile), "config.yaml")); errors.Is(err, ErrLocalProxyNode) {
		return err
	}
	return nil
}

func appliedStore(b []byte, missing bool, endpoint, revision string) (Store, error) {
	p, err := parseStore(b, missing)
	if err != nil {
		return p, err
	}
	if _, ok := p.Find(p.Selected); !ok {
		return p, errors.New("尚未选择代理节点，请在网络与代理中添加来源并选择节点")
	}
	if _, err := p.Route(); err != nil {
		return p, err
	}
	if revision == "" || p.Revision == "" || revision != p.Revision || endpoint != p.Endpoint() {
		return p, errors.New("代理节点尚未应用或配置不一致，请重新应用节点；外网保持关闭")
	}
	return p, nil
}

// StoreReader answers Load and Applied for callers that repeat them every few
// seconds. It still reads the store on every call, so any change to its bytes
// (an in-place rewrite that keeps the modification time, a replacement or a
// removal) is decided on that call; only identical bytes reuse the parsed
// store and earlier Applied decisions. A missing store is never cached: Load
// gives it a fresh random credential. The returned Store is shared and must
// not be modified. Safe for concurrent use.
type StoreReader struct {
	mu      sync.Mutex
	valid   bool
	profile string
	sum     [sha256.Size]byte
	store   Store
	err     error
	applied map[[2]string]error
}

// read refreshes the cached parse unless the bytes are unchanged.
func (r *StoreReader) read(profile string) (b []byte, missing bool, err error) {
	b, missing, err = readStore(profile)
	if err != nil || missing {
		r.valid = false
		return b, missing, err
	}
	sum := sha256.Sum256(b)
	if !r.valid || r.profile != profile || r.sum != sum {
		r.store, r.err = parseStore(b, false)
		r.valid, r.profile, r.sum, r.applied = true, profile, sum, map[[2]string]error{}
	}
	return b, false, nil
}

func (r *StoreReader) Load(profile string) (Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, missing, err := r.read(profile)
	if err != nil {
		return Store{}, err
	}
	if missing {
		return parseStore(b, true)
	}
	return r.store, r.err
}

func (r *StoreReader) Applied(profile, endpoint, revision string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, missing, err := r.read(profile)
	if err != nil {
		return err
	}
	if missing {
		_, err = appliedStore(b, true, endpoint, revision)
		return err
	}
	key := [2]string{endpoint, revision}
	decided, ok := r.applied[key]
	if !ok {
		_, decided = appliedStore(b, false, endpoint, revision)
		if len(r.applied) < 16 {
			r.applied[key] = decided
		}
	}
	if decided != nil {
		return decided
	}
	return retiredRouteCheck(profile)
}

// AppliedCheck is Applied's error for the gate watcher's once-a-second check.
type AppliedCheck struct{ reader StoreReader }

func (c *AppliedCheck) Check(profile, endpoint, revision string) error {
	return c.reader.Applied(profile, endpoint, revision)
}

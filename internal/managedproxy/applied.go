package managedproxy

import (
	"errors"
	"os"
	"path/filepath"
)

// Applied is the single source of truth for the private endpoint used by Cage.
func Applied(profile, endpoint, revision string) (Store, error) {
	p, err := Load(profile)
	if err == nil {
		err = p.checkApplied(profile, endpoint, revision)
	}
	return p, err
}

// A previous core may still expose a retired account route even when the
// selected environment route is valid. Revocation also covers that interval.
// This decision depends on the current runtime file and host addresses.
func retiredRouteCheck(profile string) error {
	if _, err := BoundInterface(filepath.Join(Dir(profile), "config.yaml")); errors.Is(err, ErrLocalProxyNode) {
		return err
	}
	return nil
}

func (p Store) checkApplied(profile, endpoint, revision string) error {
	if _, ok := p.Find(p.Selected); !ok {
		return errors.New("尚未选择代理节点，请在网络与代理中添加来源并选择节点")
	}
	if _, err := p.Route(); err != nil {
		return err
	}
	if revision == "" || p.Revision == "" || revision != p.Revision || endpoint != p.Endpoint() {
		return errors.New("代理节点尚未应用或配置不一致，请重新应用节点；外网保持关闭")
	}
	return retiredRouteCheck(profile)
}

// StoreReader answers Load and Applied for callers that repeat them every few
// seconds. It still reads the store on every call, so any change to its bytes
// (an in-place rewrite that keeps the modification time, a replacement or a
// removal) is decided on that call. Identical bytes reuse the parsed store.
// Route safety still depends on currently assigned host addresses. A missing
// store gets a fresh random credential. The returned Store is shared and
// must not be modified. Safe for concurrent use.
type StoreReader struct {
	parsed parsedConfig[Store]
}

func (r *StoreReader) Load(profile string) (Store, error) {
	p, err := r.parsed.read(filepath.Join(Dir(profile), "sources.json"), maxStoreSize+1, func(data []byte) (Store, error) {
		return parseStore(data, false)
	})
	if os.IsNotExist(err) {
		return parseStore(nil, true)
	}
	return p, err
}

func (r *StoreReader) Applied(profile, endpoint, revision string) error {
	p, err := r.Load(profile)
	if err != nil {
		return err
	}
	return p.checkApplied(profile, endpoint, revision)
}

// AppliedCheck is Applied's error for the gate watcher's once-a-second check.
type AppliedCheck struct{ reader StoreReader }

func (c *AppliedCheck) Check(profile, endpoint, revision string) error {
	return c.reader.Applied(profile, endpoint, revision)
}

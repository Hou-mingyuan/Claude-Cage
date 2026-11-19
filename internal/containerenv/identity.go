package containerenv

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/lima-vm/lima/v2/pkg/guestidentity"
)

const guestIdentityLabel = "cn.cage.guest-identity"

// A missing label means an existing environment, whose identity we preserve.
// A present but invalid label must never silently fall back to shared defaults.
func containerIdentity(container *engineContainer) (*guestidentity.Identity, error) {
	text, exists := container.Config.Labels[guestIdentityLabel]
	if !exists {
		return nil, nil
	}
	if len(text) > 1024 {
		return nil, errors.New("invalid container guest identity")
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	var identity guestidentity.Identity
	if err := decoder.Decode(&identity); err != nil {
		return nil, errors.New("invalid container guest identity")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid container guest identity")
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if container.Config.Hostname != identity.Hostname {
		return nil, errors.New("container hostname differs from its saved identity")
	}
	return &identity, nil
}

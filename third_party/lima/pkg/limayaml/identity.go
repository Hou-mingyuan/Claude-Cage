// SPDX-License-Identifier: Apache-2.0
package limayaml

import (
	"github.com/lima-vm/lima/v2/pkg/instance/hostname"
	"github.com/lima-vm/lima/v2/pkg/limatype"
)

func GuestHostname(y *limatype.LimaYAML, name string) string {
	if y.GuestIdentity != nil {
		return y.GuestIdentity.Hostname
	}
	return hostname.FromInstName(name)
}

func GuestMACAddress(y *limatype.LimaYAML, directory string) string {
	if y.GuestIdentity != nil {
		return y.GuestIdentity.MACAddress
	}
	return MACAddress(directory)
}

// SPDX-License-Identifier: Apache-2.0

package guestidentity

import (
	"crypto/rand"
	_ "embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// Only public model names and derived aliases are embedded. Environment creation
// never downloads a catalog or reads hardware identifiers from the host.
//
//go:embed hardware.csv
var hardwareCSV string

type hardwareProfile struct {
	kind, vendor, model, prefix string
}

type hardwareCatalog struct {
	computers, motherboards []hardwareProfile
}

var loadHardwareCatalog = sync.OnceValues(func() (*hardwareCatalog, error) {
	return parseHardwareCatalog(strings.NewReader(hardwareCSV))
})

var hardwarePrefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,6}$`)

func parseHardwareCatalog(input io.Reader) (*hardwareCatalog, error) {
	reader := csv.NewReader(input)
	reader.FieldsPerRecord = 4
	header, err := reader.Read()
	if err != nil || !slices.Equal(header, []string{"kind", "vendor", "model", "prefix"}) {
		return nil, errors.New("invalid hardware catalog header")
	}
	catalog := &hardwareCatalog{}
	seen := make(map[string]bool)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid hardware catalog row")
		}
		profile := hardwareProfile{row[0], row[1], row[2], row[3]}
		if !hardwarePrefixPattern.MatchString(profile.prefix) || len(profile.model) == 0 || len(profile.model) > 256 || strings.TrimSpace(profile.model) != profile.model || strings.ContainsFunc(profile.model, unicode.IsControl) {
			return nil, errors.New("invalid hardware model or hostname prefix")
		}
		switch profile.vendor {
		case "Dell", "HP", "Lenovo", "ASRock":
		default:
			return nil, errors.New("unknown hardware catalog vendor")
		}
		key := strings.Join(row[:3], "\x00")
		if seen[key] {
			return nil, errors.New("duplicate hardware catalog model")
		}
		seen[key] = true
		switch profile.kind {
		case "desktop", "laptop", "workstation":
			catalog.computers = append(catalog.computers, profile)
		case "motherboard":
			catalog.motherboards = append(catalog.motherboards, profile)
		default:
			return nil, errors.New("unknown hardware catalog kind")
		}
	}
	if len(catalog.computers) == 0 || len(catalog.motherboards) == 0 {
		return nil, errors.New("hardware catalog is missing a required model group")
	}
	return catalog, nil
}

func randomIndex(random io.Reader, length int) (int, error) {
	if length < 1 {
		return 0, errors.New("empty hostname choice")
	}
	index, err := rand.Int(random, big.NewInt(int64(length)))
	if err != nil {
		return 0, err
	}
	return int(index.Int64()), nil
}

func randomHostnameSuffix(random io.Reader, length int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, 0, length)
	for len(result) < length {
		var value [1]byte
		if _, err := io.ReadFull(random, value[:]); err != nil {
			return "", err
		}
		// 252 is divisible by 36: reject the remaining byte values to avoid bias.
		if value[0] < 252 {
			result = append(result, alphabet[int(value[0])%len(alphabet)])
		}
	}
	return string(result), nil
}

func generateHostname(random io.Reader) (string, error) {
	catalog, err := loadHardwareCatalog()
	if err != nil {
		return "", err
	}
	// Five equally likely styles, not a claim about hardware/OS market share:
	// Windows desktop/laptop, two OEM-model slots, and a motherboard-model slot.
	style, err := randomIndex(random, 5)
	if err != nil {
		return "", err
	}
	if style < 2 {
		prefix := []string{"DESKTOP", "LAPTOP"}[style]
		suffix, err := randomHostnameSuffix(random, 7)
		if err != nil {
			return "", err
		}
		return prefix + "-" + suffix, nil
	}
	profiles := catalog.computers
	if style == 4 {
		profiles = catalog.motherboards
	}
	index, err := randomIndex(random, len(profiles))
	if err != nil {
		return "", err
	}
	extra, err := randomIndex(random, 2)
	if err != nil {
		return "", err
	}
	suffix, err := randomHostnameSuffix(random, 6+extra)
	if err != nil {
		return "", err
	}
	hostname := profiles[index].prefix + "-" + suffix
	lower, err := randomIndex(random, 2)
	if err != nil {
		return "", err
	}
	if lower == 1 {
		hostname = strings.ToLower(hostname)
	}
	if !hostnamePattern.MatchString(hostname) {
		return "", fmt.Errorf("generated hostname exceeds portable naming rules")
	}
	return hostname, nil
}

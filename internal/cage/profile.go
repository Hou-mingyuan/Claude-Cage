package cage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sylvinus/agent-vm/internal/durable"
)

type runningProfile struct {
	PID         int    `json:"pid"`
	Started     string `json:"started,omitempty"`
	Fingerprint string `json:"fingerprint"`
	JobID       string `json:"job_id,omitempty"`
}

// ProcessStartMatches reports false when pid now belongs to a process started
// at a different time than the record. After a reboot or crash the recorded
// PID can be reused by an unrelated process. Records from older versions carry
// no start time and keep the previous PID-only behavior. Where the platform
// exposes start times, Cage can always read those of its own processes; an
// unreadable one belongs to another account or a protected system process,
// such as a Windows service that took over the PID after a reboot.
func ProcessStartMatches(pid int, started string) bool {
	if started == "" {
		return true
	}
	current := processStartToken(pid)
	if current == "" {
		return processStartToken(os.Getpid()) == ""
	}
	return current == started
}

func (r runningProfile) alive() bool {
	return r.PID > 0 && profileProcessAlive(r.PID) && ProcessStartMatches(r.PID, r.Started)
}

func (c Config) Fingerprint() string {
	c.ExitPolicy = c.ExitPolicy.Normalized()
	b, _ := json.Marshal(c)
	h := sha256.New()
	h.Write(b)
	if c.DNS != (DNSOptions{}) {
		extra, _ := json.Marshal(dnsOptionsFile{FallbackURLs: c.DNS.FallbackURLs(), BootstrapURLs: c.DNS.BootstrapURLs(), PublicFallback: c.DNS.PublicFallback})
		h.Write([]byte{'\n'})
		h.Write(extra)
	}
	if c.TLSClientHelloSplit {
		h.Write([]byte("\ntls_client_hello_split=1"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c Config) RecordRunning(state, name string) error {
	if filepath.Base(name) != name {
		return errors.New("invalid cage instance name")
	}
	dir := filepath.Join(state, "lima", name)
	return c.RecordRunningAt(dir)
}

// RecordRunningAt records the network owner's identity in the runtime's real
// instance directory; container environments do not create Lima metadata.
func (c Config) RecordRunningAt(dir string) error {
	pid := os.Getpid()
	b, _ := json.Marshal(runningProfile{PID: pid, Started: processStartToken(pid), Fingerprint: c.Fingerprint(), JobID: os.Getenv(JobEnv)})
	f, err := os.CreateTemp(dir, ".running-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		// Flush before the rename: after a power loss the record is either
		// the previous one or complete, never an empty file.
		err = f.Sync()
	}
	if err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "cage-running.json")); err != nil {
		return err
	}
	return durable.SyncDir(dir)
}

// CheckRunning prevents a profile edit from silently leaving a running VM on
// its previous proxy or locale. Stopping and listing remain possible.
func (c Config) CheckRunning(state string) error {
	files, err := filepath.Glob(filepath.Join(state, "lima", "*", "cage-running.json"))
	if err != nil {
		return err
	}
	containerRecords, err := filepath.Glob(filepath.Join(state, "container", "*", "cage-running.json"))
	if err != nil {
		return err
	}
	files = append(files, containerRecords...)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			continue // The owner stopped since the listing.
		}
		if err != nil {
			return err
		}
		var r runningProfile
		if json.Unmarshal(b, &r) != nil {
			// Records are replaced whole, so only a crash or power loss before
			// this version flushed them leaves an unreadable one. No running
			// owner corresponds to it; the next start replaces it.
			continue
		}
		if r.alive() && r.Fingerprint != c.Fingerprint() {
			return fmt.Errorf("cage configuration changed while a VM is running; run cage stop before restarting")
		}
	}
	return nil
}

package fsm

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
)

// syncGuide mirrors the README files out of the operator branch into
// packages_dir, so an edit to the guide reaches the page without an image build.
//
// The guide is the one thing on the dashboard whose author is not the operator of
// the cluster. It gets rewritten because a player misread a step, and until now
// that cost a tagged commit, two image builds and a redeploy of a running world —
// which is a strong argument for not fixing the sentence. The files it writes are
// the same basenames the image build bakes in, so the read path is untouched:
// dashData stats them per request and its cache invalidates on absence as well as
// presence, which is exactly what a file that appears and disappears needs.
//
// Three properties are deliberate.
//
// It runs on the FSM's own goroutine, right after a successful Fetch. That is what
// makes it safe with no lock: one writer, and it is the same goroutine that owns
// every other write in this package. The dashboard's readers are concurrent, but
// they only ever read, and a rename is atomic for them.
//
// It writes through a temporary file and renames. A reader that catches a
// half-written README would render half a page of markdown, which is bug 3's shape
// again — v1 read state out of a working tree that was being reset underneath it.
//
// And it treats every failure as non-fatal. A guide that cannot be written is a
// stale sentence on a public page; a controller that exits over one leaves a lease
// running with nobody watching it.
func (m *Machine) syncGuide() {
	dir := m.cfg.Controller.Storage.PackagesDir
	names := m.cfg.Dashboard.GuideFiles()
	if dir == "" || len(names) == 0 {
		return
	}
	for _, name := range names {
		// Base defends the join. The names come from config, where validation
		// already refuses a separator in guide_file — but that is a rule in another
		// package, and this write must not depend on it holding.
		base := filepath.Base(name)
		if base == "." || base == ".." || base == string(filepath.Separator) {
			continue
		}
		want, err := m.bus.ReadMain(name)
		switch {
		case errors.Is(err, gitbus.ErrNotFound):
			// Not a translation anyone has written. Absence is mirrored as well as
			// presence: a README deleted from the repository has to stop being served,
			// or the page keeps showing a sentence the operator has retracted.
			m.removeGuide(dir, base)
			continue
		case err != nil:
			m.logf("fsm: reading %s from %s: %v", name, m.br.Main, err)
			continue
		}
		m.writeGuide(dir, base, want)
	}
}

// writeGuide replaces one mirrored file, and says nothing when it already agrees.
//
// The comparison is what keeps this off the disk on every poll: the guide changes
// a few times a year and the poll runs every few minutes, so all but a handful of
// passes have nothing to do. It also keeps the modification time still, which the
// dashboard's cache is keyed by — rewriting identical bytes would invalidate every
// locale's cached copy four times an hour for no reason.
func (m *Machine) writeGuide(dir, base string, want []byte) {
	path := filepath.Join(dir, base)
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return
	}
	tmp, err := os.CreateTemp(dir, base+".*")
	if err != nil {
		m.logf("fsm: mirroring %s: %v", base, err)
		return
	}
	name := tmp.Name()
	// Best-effort cleanup on every path that does not reach the rename. Removing a
	// name that was already renamed away is harmless.
	defer func() {
		tmp.Close()
		os.Remove(name)
	}()
	if _, err := tmp.Write(want); err != nil {
		m.logf("fsm: mirroring %s: %v", base, err)
		return
	}
	if err := tmp.Close(); err != nil {
		m.logf("fsm: mirroring %s: %v", base, err)
		return
	}
	// 0o644, not the 0o600 CreateTemp gives: this is a file served to the public,
	// and the container may not always read it as the user that wrote it.
	if err := os.Chmod(name, 0o644); err != nil {
		m.logf("fsm: mirroring %s: %v", base, err)
		return
	}
	if err := os.Rename(name, path); err != nil {
		m.logf("fsm: mirroring %s: %v", base, err)
		return
	}
	m.logf("fsm: guide %s updated from %s (%d bytes)", base, m.br.Main, len(want))
}

// removeGuide deletes a mirrored file that the repository no longer has.
//
// A file that was never there is not an event, which is the common case: most
// deployments configure two locales and translate one, so the fallback name is
// absent on every poll forever and must not be mentioned.
func (m *Machine) removeGuide(dir, base string) {
	err := os.Remove(filepath.Join(dir, base))
	switch {
	case err == nil:
		m.logf("fsm: guide %s removed; it is no longer in %s", base, m.br.Main)
	case errors.Is(err, os.ErrNotExist):
	default:
		m.logf("fsm: removing %s: %v", base, err)
	}
}

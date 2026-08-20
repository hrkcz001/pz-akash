package state

import "time"

// Normalizer is implemented by documents whose invariants cannot be expressed in
// the type system and therefore have to be re-established after a decode.
//
// Unmarshal calls Normalize on every read, so the guarantee is a boundary
// property rather than a convention: any Controller, Agent or Backups that came
// off disk, out of a git ref, or over HTTP has already been through it. A caller
// cannot forget, because there is no read path that skips it.
//
// Normalize never fails. It rewrites the offending field to the most conservative
// value that is still true and appends a Repair, in keeping with rule 3 in the
// package doc.
type Normalizer interface {
	Normalize(r *Repairs)
}

// Normalize enforces the Controller invariants.
func (c *Controller) Normalize(r *Repairs) {
	if c.Version == 0 {
		c.Version = DocVersion
	} else if c.Version > DocVersion {
		// Not repaired, because we cannot know what a newer writer meant. The FSM
		// treats this as read-only: writing back would silently drop the fields
		// this build does not have, and one of them could be a dseq.
		r.add("version", "document is version %d, this build understands %d; treat as read-only",
			c.Version, DocVersion)
	}

	// A lease with no dseq is the dangerous shape: Billing() statuses would keep
	// reporting that money is draining while the reconciler had nothing to look
	// up. Dropping it turns an unanswerable question into a visible one.
	if c.Lease != nil && c.Lease.DSeq == "" {
		r.add("lease", "present but has no dseq; dropped, reconcile against Akash before deploying")
		c.Lease = nil
	}

	if !c.Intent.Valid() {
		r.add("intent", "unusable value %q, treated as stopped", c.Intent)
		c.Intent = IntentStopped
	}

	if !c.Status.Valid() {
		// Same reasoning as importStatus: "we do not know, and something may be
		// billing" is honestly failed, not offline.
		bad := c.Status
		if c.Lease != nil {
			c.Status = StatusFailed
			r.add("status", "unusable value %q with an active dseq; needs reconciliation", bad)
		} else {
			c.Status = StatusOffline
			r.add("status", "unusable value %q with no active dseq; assumed offline", bad)
		}
	}

	// A scheduled stop of "the zero time" is in the past forever, so it would
	// halt the server on the first tick after every boot.
	if c.StopAt != nil && c.StopAt.Zero() {
		r.add("stop_at", "present but empty; dropped rather than firing immediately")
		c.StopAt = nil
	}

	if c.ProcessedSHAs == nil {
		c.ProcessedSHAs = []string{}
	}
	kept := c.ProcessedSHAs[:0]
	dropped := 0
	seen := make(map[string]bool, len(c.ProcessedSHAs))
	for _, sha := range c.ProcessedSHAs {
		if sha == "" || seen[sha] {
			dropped++
			continue
		}
		seen[sha] = true
		kept = append(kept, sha)
	}
	c.ProcessedSHAs = kept
	if dropped > 0 {
		r.add("processed_shas", "removed %d empty or duplicate entr(y/ies)", dropped)
	}
	// An oversized ring is not corruption, but leaving it would let the document
	// grow without bound across reads.
	if n := len(c.ProcessedSHAs); n > ProcessedSHACap {
		c.ProcessedSHAs = append([]string{}, c.ProcessedSHAs[n-ProcessedSHACap:]...)
		r.add("processed_shas", "trimmed %d entries to the %d most recent", n, ProcessedSHACap)
	}

	if c.RestoreTarget != "" && !IsBackupName(c.RestoreTarget) {
		r.add("restore_target", "%q is not a backup filename; dropped", c.RestoreTarget)
		c.RestoreTarget = ""
	}
	// A pin with nothing pinned would suppress the automatic follow forever while
	// naming no archive — a server that never restores, reported as one that does.
	if c.RestorePinned && c.RestoreTarget == "" {
		r.add("restore_pinned", "set with no restore_target; cleared")
		c.RestorePinned = false
	}

	// A request with no ID cannot be matched by any report, so keeping it would
	// block every future backup — including the one a halt is waiting for — while
	// looking like work in progress.
	if c.BackupRequest != nil && c.BackupRequest.ID == "" {
		r.add("backup_request", "has no id and can never be answered; dropped")
		c.BackupRequest = nil
	}
}

// Normalize enforces the Agent invariants.
func (a *Agent) Normalize(r *Repairs) {
	if a.Version == 0 {
		a.Version = DocVersion
	} else if a.Version > DocVersion {
		r.add("version", "document is version %d, this build understands %d; treat as read-only",
			a.Version, DocVersion)
	}

	if !a.Phase.Valid() {
		// Crashed is the conservative reading: it is parked, so nothing here
		// claims the server is usable, and ImpliedStatus() is "" so the
		// controller's own view stays authoritative until the agent reports again.
		r.add("phase", "unusable value %q, treated as crashed", a.Phase)
		a.Phase = PhaseCrashed
	}

	// This is the structural half of the player-count fix. A count is a
	// measurement or it is nothing; a number with no timestamp is a value some
	// code path made up. v1 hardcoded `"players_count": 0` at eleven write sites,
	// none of which had measured anything — under this rule that document reads
	// as unknown, which is what it always meant.
	switch {
	case a.PlayersCount < 0:
		a.PlayersCount = PlayersUnknown
		a.PlayersAt = Stamp{}
	case a.PlayersAt.Zero():
		r.add("players_count", "%d with no players_at timestamp is not a measurement; treated as unknown",
			a.PlayersCount)
		a.PlayersCount = PlayersUnknown
	}

	if a.Restarts < 0 {
		r.add("restarts", "negative (%d), reset to 0", a.Restarts)
		a.Restarts = 0
	}

	if a.Backup != nil {
		// An unkeyed report cannot be matched to a request, so accepting it is how
		// a halt ends up satisfied by a backup that started before the halt did.
		if a.Backup.RequestID == "" {
			r.add("backup", "report has no request_id and cannot be matched to a request; dropped")
			a.Backup = nil
		} else if !a.Backup.State.Valid() {
			r.add("backup.state", "unusable value %q, treated as failed", a.Backup.State)
			a.Backup.State = BackupFailed
		}
	}
	if a.Backup != nil && a.Backup.State == BackupDone && a.Backup.Name == "" {
		r.add("backup", "reports done with no filename; treated as failed")
		a.Backup.State = BackupFailed
		a.Backup.Error = "report claimed success but named no archive"
	}
}

// Valid reports whether s is a backup state this version knows.
func (s BackupState) Valid() bool {
	switch s {
	case BackupRunning, BackupDone, BackupFailed:
		return true
	}
	return false
}

// Normalize enforces the Backups invariants: every entry names a real backup
// file, names are unique, and the order is newest first.
func (b *Backups) Normalize(r *Repairs) {
	if b.Version == 0 {
		b.Version = DocVersion
	} else if b.Version > DocVersion {
		r.add("version", "index is version %d, this build understands %d; treat as read-only",
			b.Version, DocVersion)
	}
	if b.Items == nil {
		b.Items = []Backup{}
	}

	kept := b.Items[:0]
	seen := make(map[string]bool, len(b.Items))
	for _, e := range b.Items {
		switch {
		case !IsBackupName(e.Name):
			r.add("items", "dropped entry %q: not a backup filename", e.Name)
			continue
		case seen[e.Name]:
			r.add("items", "dropped duplicate entry %q", e.Name)
			continue
		}
		if e.Size < 0 {
			r.add("items."+e.Name+".size", "negative (%d), reset to 0", e.Size)
			e.Size = 0
		}
		// A missing instant is recoverable from the filename, and an entry
		// without one is invisible to the age rule. Derive it in UTC, which is
		// what v1's clock used, and let the caller re-stamp if it knows better.
		if e.CreatedAt.Zero() {
			if when, err := ParseBackupName(e.Name, time.UTC); err == nil {
				e.CreatedAt = At(when)
				r.add("items."+e.Name+".created_at", "missing; derived from the filename as UTC")
			}
		}
		seen[e.Name] = true
		kept = append(kept, e)
	}
	b.Items = kept
	b.Sort()
}

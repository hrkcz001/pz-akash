package fsm

import (
	"context"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Trigger names. A trigger is a file in the triggers directory; its presence is
// the request, and its content is optional detail.
const (
	// TriggerStart deploys. The only thing that leaves offline, and the only
	// thing that clears a failure.
	TriggerStart = "start"
	// TriggerHalt shuts down: final backup, stop, close the lease.
	TriggerHalt = "halt"
	// TriggerBackup asks for a backup now, without stopping.
	TriggerBackup = "backup"
	// TriggerStopAt schedules a shutdown. Body is an RFC 3339 timestamp, or one
	// of the bare forms parseStopAt accepts, interpreted in identity.timezone.
	TriggerStopAt = "stop_at"
	// TriggerRestore names the archive the next boot must restore. Body is a
	// backup name; an empty body means "next boot starts a fresh world", which
	// has to be spelled out rather than inferred.
	TriggerRestore = "restore"
)

// stopAtFormats are accepted in a stop_at body, in order. RFC 3339 first, so an
// explicit offset always wins; the bare forms are read in identity.timezone,
// which is the same rule the dashboard renders by.
var stopAtFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"15:04",
}

// consumeTriggers reads the triggers directory, deletes everything it found in a
// single commit, and only then acts.
//
// The order is the whole point, and it is the reverse of v1's. v1 acted first and
// cleared the sentinel afterwards, so anything that interrupted it between those
// two steps — a crash, a slow push, a webhook arriving mid-halt — re-ran the
// action on the next tick. That is one of the two mechanisms that turned a single
// halt into several, and it is why one halt produced several backups.
//
// The cost of consuming first is a crash in the window between the delete
// commit and the action, which loses that trigger. That is the better failure:
// a request that has to be re-pushed is visible and cheap, while an action
// replayed an unknown number of times is neither. Everything the triggers set
// beyond that is recoverable from the document, so the only truly lossy case is
// the edge itself.
func (m *Machine) consumeTriggers(ctx context.Context) {
	found, err := m.bus.Triggers()
	if err != nil {
		m.logf("fsm: read triggers: %v", err)
		return
	}
	if len(found) == 0 {
		return
	}

	names := make([]string, 0, len(found))
	act := make(map[string]gitbus.Trigger, len(found))
	for _, t := range found {
		if !knownTrigger(t.Name) {
			// Left in place rather than deleted: it may be a typo the operator
			// wants to see, or a file some other tool owns. Warned once so a
			// permanent stray does not print on every poll.
			if !m.warned["trigger:"+t.Name] {
				m.warned["trigger:"+t.Name] = true
				m.logf("fsm: ignoring unknown trigger %q (left in place)", t.Name)
			}
			continue
		}
		names = append(names, t.Name)
		act[t.Name] = t
	}
	if len(names) == 0 {
		return
	}

	sha, err := m.bus.ConsumeTriggers(ctx, names)
	if err != nil {
		// Not acted on. The trigger files are still there, so the next poll tries
		// again — which is the safe direction: a request that has not been
		// consumed has not been answered either.
		m.logf("fsm: could not consume trigger(s) %s: %v", strings.Join(names, ", "), err)
		return
	}
	if sha != "" {
		// Recorded immediately, not at the next flush: this is the commit whose
		// webhook delivery would otherwise come back to us as a fresh request.
		m.doc.MarkProcessed(sha)
		m.dirty("consumed " + strings.Join(names, ", "))
	}
	m.logf("fsm: consumed trigger(s): %s", strings.Join(names, ", "))

	// Field setters run before the actions that read those fields, so
	// `restore` + `start` pushed together boots the named archive rather than
	// whatever the previous cycle left behind.
	if t, ok := act[TriggerRestore]; ok {
		m.applyRestore(t)
	}
	if t, ok := act[TriggerStopAt]; ok {
		m.applyStopAt(t)
	}

	// Halt outranks start: pushed together, the safe reading is "do not bring it
	// up", and the operator can always push start again.
	switch {
	case act[TriggerHalt].Name != "":
		m.beginHalt(ctx, "halt trigger")
	case act[TriggerStart].Name != "":
		m.beginDeploy(ctx, "start trigger")
	}

	// A backup asked for alongside a halt is already covered by the halt's own
	// final backup, and asking twice would only be refused.
	if act[TriggerBackup].Name != "" && act[TriggerHalt].Name == "" {
		m.operatorBackup(ctx)
	}
}

func knownTrigger(name string) bool {
	switch name {
	case TriggerStart, TriggerHalt, TriggerBackup, TriggerStopAt, TriggerRestore:
		return true
	}
	return false
}

// operatorBackup handles an on-demand backup request, which is only meaningful
// while there is a server to back up.
func (m *Machine) operatorBackup(ctx context.Context) {
	switch m.doc.Status {
	case state.StatusOnline, state.StatusBackingUp:
		// requestBackup makes the transition to backing_up itself, and logs the
		// refusal if a periodic backup — or a second push of the same trigger — is
		// already in flight.
		m.requestBackup(ctx, "operator")
	default:
		m.logf("fsm: backup trigger ignored: status is %s", m.doc.Status)
		m.doc.LastError = "backup requested while " + string(m.doc.Status)
		m.dirty("backup trigger ignored while " + string(m.doc.Status))
	}
}

// applyRestore points the next boot at a named archive, and records that an
// operator was the one who chose it.
//
// The name is checked against the index, because an unknown target is the one
// mistake with a silent and unrecoverable failure mode: the agent would find
// nothing to download and boot a fresh world over a perfectly good save. v1 let
// restore_target drift until it named a file that had never existed.
//
// Naming an archive also pins it. Under the latest policy every completed backup
// moves the target, so without the pin an operator's choice would survive only
// until the next periodic backup — and the reason to make that choice at all is
// that the recent backups are the bad ones. The body "latest" (or "auto") releases
// the pin again, which is the only way back to following.
func (m *Machine) applyRestore(t gitbus.Trigger) {
	want := strings.TrimSpace(string(t.Body))
	if m.cfg.Backups.RestorePolicy == config.RestoreNone {
		// Refused rather than applied-and-ignored: under this policy every boot is a
		// fresh world, so an operator who asks for a restore is going to lose one if
		// nobody tells them the request went nowhere.
		if want == "" {
			m.logf("fsm: restore trigger is empty, which is what backups.restore_policy=%s already does",
				config.RestoreNone)
			return
		}
		m.logf("fsm: restore target %q refused: backups.restore_policy is %s", want, config.RestoreNone)
		m.doc.LastError = "restore refused: backups.restore_policy is " + config.RestoreNone
		m.dirty("refused restore target " + want)
		return
	}
	if want == "" {
		// Spelled out rather than inferred: an empty restore trigger is how an
		// operator says "start a new world", and it must not be confused with a
		// trigger whose body failed to arrive.
		if m.doc.RestoreTarget == "" {
			m.logf("fsm: restore trigger is empty and no target was set; the next boot starts a fresh world")
			return
		}
		m.logf("fsm: restore trigger is empty; clearing target %q, the next boot starts a fresh world",
			m.doc.RestoreTarget)
		m.doc.RestoreTarget = ""
		m.doc.RestorePinned = false
		m.dirty("cleared restore_target")
		return
	}
	if strings.EqualFold(want, config.RestoreLatest) || strings.EqualFold(want, "auto") {
		m.followLatest()
		return
	}
	if !m.idx.Has(want) {
		m.logf("fsm: restore target %q is not in the index (%d known); keeping %q",
			want, len(m.idx.Items), m.doc.RestoreTarget)
		m.doc.LastError = "restore target " + want + " is not in the backup index"
		m.dirty("rejected restore target " + want)
		return
	}
	if m.doc.RestoreTarget == want && m.doc.RestorePinned {
		return
	}
	m.logf("fsm: restore target %q -> %q (pinned)", m.doc.RestoreTarget, want)
	m.doc.RestoreTarget = want
	m.doc.RestorePinned = true
	m.dirty("pinned restore_target " + want)
}

// followLatest releases the pin and points the target at the newest known backup.
//
// Both halves are needed. Clearing the pin alone would leave the stale target in
// place until the next backup completed — and under the pinned policy, which has
// no automatic follow, forever.
func (m *Machine) followLatest() {
	newest := m.idx.Newest()
	if newest == nil {
		if m.doc.RestoreTarget == "" {
			m.logf("fsm: restore trigger asked to follow the newest backup, but the index is empty")
			return
		}
		m.logf("fsm: restore trigger asked to follow the newest backup and the index is empty; clearing target %q",
			m.doc.RestoreTarget)
		m.doc.RestoreTarget = ""
		m.doc.RestorePinned = false
		m.dirty("cleared restore_target: the index is empty")
		return
	}
	// Under the pinned policy nothing follows anything, so the flag stays set: the
	// name is applied once and will not move, and recording it as unpinned would
	// describe a document that does not exist.
	pinned := m.cfg.Backups.RestorePolicy == config.RestorePinned
	if m.doc.RestoreTarget == newest.Name && m.doc.RestorePinned == pinned {
		return
	}
	if pinned {
		m.logf("fsm: restore target %q -> %q; backups.restore_policy is %s, so it will not move again",
			m.doc.RestoreTarget, newest.Name, config.RestorePinned)
	} else {
		m.logf("fsm: restore target %q -> %q (following the newest again)",
			m.doc.RestoreTarget, newest.Name)
	}
	m.doc.RestoreTarget = newest.Name
	m.doc.RestorePinned = pinned
	m.dirty("restore_target " + newest.Name)
}

// applyStopAt sets or clears the scheduled shutdown.
func (m *Machine) applyStopAt(t gitbus.Trigger) {
	body := strings.TrimSpace(string(t.Body))
	if body == "" || strings.EqualFold(body, "none") || strings.EqualFold(body, "never") {
		if m.doc.StopAt == nil {
			m.logf("fsm: stop_at trigger is empty and no stop was scheduled")
			return
		}
		m.logf("fsm: cleared the scheduled stop (%s)", m.doc.StopAt)
		m.doc.StopAt = nil
		m.dirty("cleared stop_at")
		return
	}
	when, err := m.parseStopAt(body)
	if err != nil {
		m.logf("fsm: stop_at %q: %v", body, err)
		m.doc.LastError = "stop_at " + body + ": " + err.Error()
		m.dirty("rejected stop_at " + body)
		return
	}
	if !when.After(m.now()) {
		// Accepted anyway: a time already past means "stop now", and refusing it
		// would be a surprising way to answer an operator who is a minute late.
		m.logf("fsm: stop_at %s is already past; stopping at the next tick", when.Format(time.RFC3339))
	}
	s := state.At(when).In(m.loc)
	m.doc.StopAt = &s
	m.logf("fsm: scheduled stop at %s", s)
	m.dirty("stop_at " + s.String())
}

// parseStopAt reads a stop_at body.
//
// A form with no offset is read in identity.timezone, never in the host's local
// time. That is the rule the whole system is built on: the container has no TZ
// set, so "22:00" interpreted locally would silently mean 22:00 UTC — an hour or
// two off what the operator meant, and off by a different amount in summer.
func (m *Machine) parseStopAt(body string) (time.Time, error) {
	var firstErr error
	for _, layout := range stopAtFormats {
		t, err := time.ParseInLocation(layout, body, m.loc)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if layout == "15:04" {
			// A bare time means the next occurrence of it, which is what someone
			// writing "22:00" means — today if it is still ahead, tomorrow if not.
			now := m.now().In(m.loc)
			t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, m.loc)
			if !t.After(now) {
				t = t.AddDate(0, 0, 1)
			}
		}
		return t, nil
	}
	return time.Time{}, firstErr
}

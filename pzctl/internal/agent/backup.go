package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// backupJob is one in-flight backup. At most one exists at a time: there is one
// PZ process and one Saves directory, so a second concurrent archive could only
// be a torn copy of the first.
type backupJob struct {
	req     state.BackupRequest
	name    string
	started time.Time
	done    chan *state.BackupReport
}

// startBackup answers a controller backup request.
//
// The report it eventually publishes carries the request's ID, and that is the
// whole of the bug 4 fix. In v1 the two sides communicated through a flag and a
// directory listing: the controller set backup_requested and then waited for
// "a new archive to appear", so a periodic backup that had begun before a halt
// request could sign off on that halt while saving a world several minutes stale
// — and a request whose archive failed to upload was answered by the *next*
// unrelated backup instead of being reported as failed.
func (a *Agent) startBackup(ctx context.Context, req state.BackupRequest, index *state.Backups) {
	taken := func(name string) bool {
		if index != nil && index.Has(name) {
			return true
		}
		_, err := os.Stat(filepath.Join(a.cfg.Agent.Paths.WorkDir, name))
		return err == nil
	}
	name := state.NewBackupName(time.Now().In(a.loc), a.loc, taken)

	job := &backupJob{
		req:     req,
		name:    name,
		started: time.Now(),
		done:    make(chan *state.BackupReport, 1),
	}
	a.backup = job

	now := a.now()
	a.doc.Backup = &state.BackupReport{
		RequestID: req.ID,
		State:     state.BackupRunning,
		Name:      name,
		StartedAt: now,
	}
	// Only report saving while PZ is actually up. A backup taken from a parked
	// container is a legitimate operation — the world on disk is complete and the
	// controller may well be asking precisely because it is about to close the
	// lease — but claiming "saving" would tell the controller the server came back.
	if a.pz != nil && a.pz.Running() {
		a.setPhase(state.PhaseSaving, "backup "+req.Reason)
	}
	a.mark("backup " + name + " started")
	a.publish(ctx, true)
	a.log("backup: %s requested (%s, id %s)", name, req.Reason, req.ID)

	pz := a.pz
	go a.runBackup(job, pz)
}

// runBackup is the goroutine: save, zip, upload. It touches no Agent field
// except the immutable config and the client, and reports back over job.done.
func (a *Agent) runBackup(job *backupJob, pz *pzProcess) {
	rep := &state.BackupReport{
		RequestID: job.req.ID,
		State:     state.BackupRunning,
		Name:      job.name,
		StartedAt: state.At(job.started.In(a.loc)),
	}
	fail := func(err error) {
		rep.State = state.BackupFailed
		rep.Error = err.Error()
		rep.EndedAt = a.now()
		job.done <- rep
	}

	// A save first, always. v1 zipped the world with no save at all when RCON was
	// unavailable, which it always was — so every backup it took was as stale as
	// the game's own last autosave.
	if pz != nil && pz.Running() {
		if err := a.saveOn(pz); err != nil {
			// Not fatal. What is on disk is at worst the last autosave, which is
			// exactly what v1 always produced, so a backup of it is still worth
			// having — and refusing here would leave a halt with nothing at all.
			a.log("backup: %v (archiving the world as it is on disk)", err)
			rep.Error = "save not confirmed: " + err.Error()
		}
	}

	saves := filepath.Join(a.cfg.Agent.Paths.DataDir, "Saves")
	archive := filepath.Join(a.cfg.Agent.Paths.WorkDir, job.name)
	// Ensured here, not left to boot: an agent that parked without booting — the
	// container Kubernetes restarted mid-halt — has no work directory, and this is
	// precisely the agent the controller is asking for the session before it closes
	// the lease. Failing here would throw that session away.
	if err := os.MkdirAll(a.cfg.Agent.Paths.WorkDir, 0o755); err != nil {
		fail(fmt.Errorf("create %s: %w", a.cfg.Agent.Paths.WorkDir, err))
		return
	}
	size, sum, err := zipDir(saves, archive)
	if err != nil {
		fail(fmt.Errorf("archive %s: %w", saves, err))
		return
	}
	// Removed on every path: the archive is a second copy of the world on a disk
	// sized for one, and the controller has it once the upload returns.
	defer os.Remove(archive)
	rep.Size, rep.SHA256 = size, sum
	a.log("backup: %s built (%d bytes, sha256 %s) in %v", job.name, size, sum[:12], time.Since(job.started).Truncate(time.Second))

	if size > a.cfg.Backups.UploadMaxBytes {
		fail(fmt.Errorf("%s is %d bytes, over the backups.upload_max_bytes limit of %d",
			job.name, size, a.cfg.Backups.UploadMaxBytes))
		return
	}

	cli, err := a.client()
	if err != nil {
		fail(err)
		return
	}
	// The upload's own context, not the loop's: a halt cancels nothing here. The
	// controller is waiting out halt_timeout for this exact transfer, and killing
	// it at the ctx boundary would be a halt that discards the session it just
	// saved.
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Backups.HaltTimeout.D())
	defer cancel()

	res, err := cli.UploadBackup(ctx, job.name, archive, sum, job.req.ID, string(a.doc.Phase))
	if err != nil {
		fail(fmt.Errorf("upload %s: %w", job.name, err))
		return
	}
	if res.SHA256 != "" && res.SHA256 != sum {
		fail(fmt.Errorf("upload %s: the controller stored sha256 %s, we sent %s", job.name, res.SHA256, sum))
		return
	}
	rep.State = state.BackupDone
	rep.EndedAt = a.now()
	job.done <- rep
}

// finishBackup records the outcome and returns the agent to its resting phase.
func (a *Agent) finishBackup(ctx context.Context, rep *state.BackupReport) {
	a.backup = nil
	a.doc.Backup = rep
	// Marked answered whether it succeeded or failed. A failed backup that stayed
	// unanswered would be retried on every reconcile, which is a disk-filling loop
	// on the failure mode most likely to be permanent (an oversized archive).
	a.answered[rep.RequestID] = true

	switch rep.State {
	case state.BackupDone:
		a.log("backup: %s uploaded (%d bytes) in %v", rep.Name, rep.Size, rep.EndedAt.Sub(rep.StartedAt.Time).Truncate(time.Second))
		a.mark("backup " + rep.Name + " done")
	default:
		a.log("backup: %s failed: %s", rep.Name, rep.Error)
		a.doc.LastError = rep.Error
		a.mark("backup " + rep.Name + " failed")
	}

	// Back to whatever is true now. Deliberately re-derived rather than
	// remembered: a halt may have arrived while the upload was running, and the
	// next reconcile will act on it.
	switch {
	case a.pz != nil && a.pz.Running():
		if a.doc.Phase == state.PhaseSaving {
			a.setPhase(state.PhaseOnline, "backup finished")
		}
	case a.parked:
		// Leave the parked phase alone; it is the reason we are still here.
	default:
		a.setPhase(state.PhaseStopped, "backup finished with no PZ process")
	}
	a.publish(ctx, true)
}

// save is the console save on the current process, used by the shutdown path.
func (a *Agent) save() error {
	if a.pz == nil {
		return errors.New("no PZ process")
	}
	return a.saveOn(a.pz)
}

// saveOn writes the save command and waits for a confirmation line.
//
// A confirmation that never arrives is an error the caller is expected to log and
// continue past, not a reason to abandon a backup: the wording of PZ's
// acknowledgement has changed between builds, so "not confirmed" more often means
// "the config's save_confirm list is out of date" than "the save failed".
func (a *Agent) saveOn(pz *pzProcess) error {
	z := a.cfg.Agent.PZ
	if err := pz.Send(z.SaveCommand); err != nil {
		return fmt.Errorf("write %q to the console: %w", z.SaveCommand, err)
	}
	if len(z.SaveConfirm) == 0 {
		// No patterns configured: the only honest thing left is to give the save
		// its full budget before touching the files.
		a.log("save: no agent.pz.save_confirm patterns — waiting out %v", z.SaveTimeout.D())
		time.Sleep(z.SaveTimeout.D())
		return nil
	}
	line, ok := pz.waitFor(z.SaveConfirm, z.SaveTimeout.D())
	if !ok {
		return fmt.Errorf("no save confirmation within %v (watching for %s)",
			z.SaveTimeout.D(), strings.Join(z.SaveConfirm, ", "))
	}
	a.log("save: confirmed by %q", strings.TrimSpace(line))
	return nil
}

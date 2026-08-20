package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hrkcz001/pz-akash/pzctl/internal/httpapi"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// errRestore marks a boot failure that was specifically a failed restore, so Run
// can report PhaseRestoreFailed. The distinction matters to an operator: a fresh
// world booting in place of the save they asked for is data loss, and reporting
// it as a crash would invite the controller to retry into the same place.
var errRestore = errors.New("restore failed")

// boot brings the container from "just started" to "PZ running".
//
// The order is fixed and each step depends on the last: layout, then the game
// files from the controller, then the world (either restored or left as it is),
// then the config that must be in place before the JVM reads it, then launch.
func (a *Agent) boot(ctx context.Context) error {
	a.setPhase(state.PhaseStarting, "boot")
	a.publish(ctx, true)

	if err := a.prepareLayout(); err != nil {
		return err
	}
	if err := a.fetchServerFiles(ctx); err != nil {
		return err
	}
	if a.restoreTarget != "" {
		if err := a.restore(ctx, a.restoreTarget); err != nil {
			return fmt.Errorf("%w: %v", errRestore, err)
		}
	}
	if err := a.renderConfig(); err != nil {
		return err
	}
	return a.startGame(ctx)
}

// prepareLayout creates the directories PZ expects and the lowercase link, and
// empties the work directory.
func (a *Agent) prepareLayout() error {
	p := a.cfg.Agent.Paths
	for _, dir := range []string{
		p.Home, p.DataDir,
		filepath.Join(p.DataDir, "Server"),
		filepath.Join(p.DataDir, "Saves"),
		// db/ holds the player accounts SQLite file. It is created here and
		// deliberately not part of a backup — same limitation as v1, and worth
		// knowing: restoring a save does not restore accounts.
		filepath.Join(p.DataDir, "db"),
		filepath.Join(p.DataDir, "mods"),
		p.WorkDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	// Downloads and a half-built zip from a previous container are worthless and
	// could be mistaken for this boot's, so the directory starts empty. Validation
	// guarantees it is not data_dir or game_dir.
	if err := emptyDir(p.WorkDir); err != nil {
		return fmt.Errorf("clean %s: %w", p.WorkDir, err)
	}

	if err := a.linkLowercase(); err != nil {
		return err
	}
	return nil
}

// linkLowercase points paths.lowercase_link at paths.data_dir.
//
// The game reads mods from ~/Zomboid but builds some internal paths in ~/zomboid
// whatever -cachedir says. ext4 is case-sensitive and has no directory hardlinks,
// so the only way both names reach one directory is a symlink.
func (a *Agent) linkLowercase() error {
	link := a.cfg.Agent.Paths.LowercaseLink
	if link == "" {
		return nil
	}
	target := a.cfg.Agent.Paths.DataDir

	switch fi, err := os.Lstat(link); {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		if dst, err := os.Readlink(link); err == nil && dst == target {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("replace the symlink %s: %w", link, err)
		}
	case err == nil:
		// A real directory is here. Removing it could delete a world, so this
		// stops instead: it means the image or the config changed underneath us.
		return fmt.Errorf("%s exists and is not a symlink; agent.paths.lowercase_link cannot be created", link)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("stat %s: %w", link, err)
	}

	if err := os.Symlink(target, link); err != nil {
		// Not fatal on a filesystem that has no symlinks (a Windows test run
		// without developer mode). The game only needs it on Linux.
		a.log("cannot create the lowercase link %s -> %s: %v", link, target, err)
	}
	return nil
}

// fetchServerFiles downloads common.zip and server.zip from the controller and
// unpacks both into data_dir.
//
// server.zip is fetched with the server-files token because the controller
// substitutes the PZ passwords into the .ini as it serves it. That is why no
// password reaches an image layer or an Akash manifest, where it would be
// readable by the provider.
func (a *Agent) fetchServerFiles(ctx context.Context) error {
	cli, err := a.client()
	if err != nil {
		return err
	}
	a.log("boot: waiting for the controller at %s", cli.Base())
	if err := cli.WaitHealthy(ctx); err != nil {
		return fmt.Errorf("controller is not reachable: %w", err)
	}

	work := a.cfg.Agent.Paths.WorkDir
	for _, f := range []struct {
		path     string
		realm    httpapi.Realm
		optional bool
	}{
		{httpapi.PathCommonZip, httpapi.RealmPublic, true},
		{httpapi.PathServerZip, httpapi.RealmServerFiles, false},
	} {
		dst := filepath.Join(work, filepath.Base(f.path))
		n, err := cli.Download(ctx, f.path, f.realm, dst)
		if err != nil {
			// common.zip is optional because a server with no shared mods or maps
			// legitimately has none; server.zip carries the .ini and is not.
			if f.optional && IsNotFound(err) {
				a.log("boot: %s is absent — skipping", f.path)
				continue
			}
			return fmt.Errorf("download %s: %w", f.path, err)
		}
		a.log("boot: %s -> %s (%d bytes)", f.path, dst, n)
		if err := unzip(dst, a.cfg.Agent.Paths.DataDir); err != nil {
			return fmt.Errorf("unpack %s: %w", f.path, err)
		}
		// Freed immediately: work_dir shares the container's disk with the world
		// and, later, with the backup archive being built.
		if err := os.Remove(dst); err != nil {
			a.log("cannot remove %s: %v", dst, err)
		}
	}
	return nil
}

// restore replaces the world with a backup from the controller.
//
// It is only ever called at boot. Unpacking a save over a world PZ has open
// produces a mix of two worlds, so the controller's sequence is stop, set the
// target, start — and the agent enforces the "at boot" half of that.
func (a *Agent) restore(ctx context.Context, name string) error {
	if !state.IsBackupName(name) {
		return fmt.Errorf("%q is not a backup file name", name)
	}
	cli, err := a.client()
	if err != nil {
		return err
	}
	a.setPhase(state.PhaseRestoring, "restoring "+name)
	a.publish(ctx, true)

	// The index is advisory here: it gives us the digest to check against. A
	// missing entry is not a reason to refuse, because the archive itself is the
	// thing being restored and the controller served it.
	var want string
	if _, index, _, err := a.bus.ReadController(); err == nil && index != nil {
		if e := index.Find(name); e != nil {
			want = e.SHA256
		}
	}

	archive := filepath.Join(a.cfg.Agent.Paths.WorkDir, name)
	n, err := cli.Download(ctx, httpapi.BackupPath(name), httpapi.RealmBackups, archive)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer os.Remove(archive)
	a.log("restore: downloaded %s (%d bytes)", name, n)

	if want != "" {
		got, err := sha256File(archive)
		if err != nil {
			return fmt.Errorf("hash %s: %w", name, err)
		}
		if got != want {
			// Refuse rather than unpack: the previous world is still on disk and
			// intact at this point, and a truncated archive would replace it with a
			// partial one.
			return fmt.Errorf("%s is corrupt: sha256 %s, index says %s", name, got, want)
		}
	}

	saves := filepath.Join(a.cfg.Agent.Paths.DataDir, "Saves")
	// The archive holds the *contents* of Saves with relative paths, as v1's
	// `cd Saves && zip -r - .` produced. Emptying first is what makes a restore a
	// replacement rather than a merge with whatever world was here.
	if err := emptyDir(saves); err != nil {
		return fmt.Errorf("empty %s: %w", saves, err)
	}
	if err := unzip(archive, saves); err != nil {
		return fmt.Errorf("unpack %s into %s: %w", name, saves, err)
	}
	a.log("restore: %s applied to %s (%d bytes on disk)", name, saves, dirSize(saves))
	return nil
}

// renderConfig writes the PZ .ini keys config.yaml owns and the JVM heap.
//
// Both files arrive from elsewhere — the .ini inside server.zip with the
// passwords already substituted, the .json from the image — so both are patched
// in place rather than generated. Overwriting either wholesale would drop the
// secrets or the launcher's own vmArgs.
func (a *Agent) renderConfig() error {
	p := a.cfg.Agent.Paths
	ini := filepath.Join(p.DataDir, "Server", a.cfg.Identity.ServerName+".ini")
	changed, err := renderServerINI(ini, a.cfg)
	if err != nil {
		return fmt.Errorf("render %s: %w", ini, err)
	}
	if len(changed) > 0 {
		a.log("config: %s updated %d key(s): %v", filepath.Base(ini), len(changed), changed)
	}

	// pzexe silently drops CLI options it does not recognise, -Xmx among them, so
	// the heap has to go in the launcher's own vmArgs. A 16Gi container running on
	// the JVM's default heap is how v1 spent months OOM-killing itself.
	js := filepath.Join(p.GameDir, "ProjectZomboid64.json")
	if patched, err := patchLaunchJSON(js, a.cfg.Server.MemoryMax, a.cfg.Server.MemoryMin); err != nil {
		a.log("cannot patch %s: %v (continuing with the image's defaults)", js, err)
	} else if patched {
		a.log("config: %s heap set to -Xmx%s/-Xms%s", filepath.Base(js), a.cfg.Server.MemoryMax, a.cfg.Server.MemoryMin)
	}
	return nil
}

// startGame launches PZ. Also the relaunch path after a crash.
func (a *Agent) startGame(ctx context.Context) error {
	if a.parked {
		// Belt and braces. Every caller checks, but this is the one invariant whose
		// violation resurrects a halted world, so it is enforced where the process
		// is actually created.
		a.log("not launching PZ: parked (%s)", a.parkWhy)
		return nil
	}
	p := a.cfg.Agent.Paths

	if a.launcher == "" {
		launcher, err := findLauncher(p.GameDir, a.cfg.Agent.PZ.LaunchScripts)
		if err != nil {
			return err
		}
		a.launcher = launcher
		a.log("boot: launcher %s", launcher)
	}
	// The image ships these without the bit set often enough that v1 chmod'ed them
	// on every boot; doing the same keeps a rebuilt image from failing here.
	for _, exe := range []string{
		a.launcher,
		filepath.Join(filepath.Dir(a.launcher), "ProjectZomboid64"),
		filepath.Join(filepath.Dir(a.launcher), "jre64", "bin", "java"),
	} {
		if err := os.Chmod(exe, 0o755); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.log("cannot chmod %s: %v", exe, err)
		}
	}

	args := []string{
		"-servername", a.cfg.Identity.ServerName,
		"-cachedir=" + p.DataDir,
	}
	// No -adminpassword: the agent holds no admin secret, on purpose. The password
	// is substituted into the .ini by the controller, which means it never appears
	// in this container's process list — where v1 put it, and where any shell in
	// the container could read it out of /proc.
	args = append(args, a.cfg.Agent.PZ.ExtraArgs...)

	pz, err := startPZ(a.procCtx, a.launcher, args, a.cfg.Agent.PZ, a.logPath(), a.events, a.log)
	if err != nil {
		return fmt.Errorf("launch PZ: %w", err)
	}
	a.pz = pz
	a.setPhase(state.PhaseStarting, "PZ launched")
	a.doc.SetPlayers(state.PlayersUnknown, a.now())
	a.publish(ctx, true)
	return nil
}

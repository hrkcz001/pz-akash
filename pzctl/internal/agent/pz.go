package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// This file owns the PZ process: one launch, one stdin, one merged output
// stream, and the rules for stopping it.
//
// The design difference from v1 is that the agent is the process's parent. v1's
// entrypoint.sh redirected PZ's output into a file, tailed the file to find the
// ready banner, and had to remember a line offset across restarts so an old
// banner would not be re-matched. It also had no way to write to PZ's stdin,
// which is why v1 needed RCON for saves and never actually managed to read a
// player count (bug 1). Holding the pipes makes both of those ordinary.

type eventKind int

const (
	evOnline  eventKind = iota // the ready banner appeared
	evPlayers                  // a player count was recognised
	evExit                     // the process ended
)

type event struct {
	kind    eventKind
	players int
	exitErr error
	code    int
}

// pzProcess is a running game server.
type pzProcess struct {
	cfg config.AgentPZ

	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu       sync.Mutex
	matchers []*matcher
	closed   bool

	done chan struct{} // closed once Wait has returned
}

// matcher is a one-shot wait for a line, used by the save path. Registering it
// with the scanner rather than re-reading the log is what keeps "did the save
// finish" from depending on file offsets.
type matcher struct {
	want []string
	hit  chan string
}

// findLauncher locates the game's launch script under dir, trying the configured
// names in order. Order matters: v1 used `find -name a -o -name b | head -1`,
// which returns whichever the filesystem yields first, so which launcher ran
// depended on directory layout.
func findLauncher(dir string, names []string) (string, error) {
	for _, name := range names {
		var found string
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // an unreadable subtree is not a reason to stop looking
			}
			if !d.IsDir() && d.Name() == name {
				found = p
				return fs.SkipAll
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.SkipAll) {
			return "", err
		}
		if found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("no launcher named %s found under %s", strings.Join(names, " or "), dir)
}

// startPZ launches the server and begins streaming its output as events.
//
// stdout and stderr are merged into one pipe on purpose: PZ writes the ready
// banner to stdout and some startup diagnostics to stderr, and two scanners
// would interleave them unpredictably in the mirrored log.
func startPZ(ctx context.Context, launcher string, args []string, cfg config.AgentPZ, logPath string, out chan<- event, logf func(string, ...any)) (*pzProcess, error) {
	cmd := exec.CommandContext(ctx, launcher, args...)
	cmd.Dir = filepath.Dir(launcher)
	// PZ resolves its own paths from the launcher's directory and from HOME, so
	// the environment is inherited rather than cleared.
	cmd.Env = os.Environ()
	configureProcAttr(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout

	p := &pzProcess{cfg: cfg, cmd: cmd, stdin: stdin, done: make(chan struct{})}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch %s: %w", launcher, err)
	}
	logf("pz: started %s (pid %d)", launcher, cmd.Process.Pid)

	go p.scan(pipe, logPath, out, logf)
	go func() {
		err := cmd.Wait()
		close(p.done)
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		out <- event{kind: evExit, exitErr: err, code: code}
	}()
	return p, nil
}

// scan reads the merged output until EOF, mirroring every line and turning the
// interesting ones into events.
func (p *pzProcess) scan(pipe io.ReadCloser, logPath string, out chan<- event, logf func(string, ...any)) {
	defer pipe.Close()

	var mirror io.WriteCloser
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			mirror = f
			defer f.Close()
		} else {
			logf("pz: cannot mirror output to %s: %v", logPath, err)
		}
	}

	sc := bufio.NewScanner(pipe)
	// PZ logs stack traces and mod lists; the default 64KiB line limit is
	// generous but a single long line must not end the scan.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	online := false
	for sc.Scan() {
		line := sc.Text()
		if mirror != nil {
			fmt.Fprintln(mirror, line)
		}
		// Also to the agent's own stdout, so `kubectl logs` and the provider's
		// log view show the game console without an operator needing a shell.
		fmt.Println("pz| " + line)

		p.notify(line)

		if !online && p.cfg.ReadyBanner != "" && strings.Contains(line, p.cfg.ReadyBanner) {
			online = true
			out <- event{kind: evOnline}
		}
		if n, ok := parsePlayers(line); ok {
			out <- event{kind: evPlayers, players: n}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		logf("pz: output scan ended: %v", err)
	}
}

// notify delivers a line to any waiter registered by waitFor.
func (p *pzProcess) notify(line string) {
	lower := strings.ToLower(line)
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.matchers[:0]
	for _, m := range p.matchers {
		matched := false
		for _, w := range m.want {
			if strings.Contains(lower, w) {
				matched = true
				break
			}
		}
		if matched {
			select {
			case m.hit <- line:
			default:
			}
			continue
		}
		kept = append(kept, m)
	}
	p.matchers = kept
}

// waitFor blocks until a line containing one of want (case-insensitive) appears,
// the timeout expires, or the process exits. It reports whether a line matched,
// because for a save the timeout is a warning rather than a failure: proceeding
// with a possibly-unflushed world is better than never producing a backup.
func (p *pzProcess) waitFor(want []string, timeout time.Duration) (string, bool) {
	if len(want) == 0 {
		return "", false
	}
	m := &matcher{hit: make(chan string, 1)}
	for _, w := range want {
		m.want = append(m.want, strings.ToLower(w))
	}
	p.mu.Lock()
	p.matchers = append(p.matchers, m)
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for i, x := range p.matchers {
			if x == m {
				p.matchers = append(p.matchers[:i], p.matchers[i+1:]...)
				return
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line := <-m.hit:
		return line, true
	case <-p.done:
		return "", false
	case <-timer.C:
		return "", false
	}
}

// Send writes one console command. An error here means the process is gone,
// which the exit event will report; callers log and carry on rather than
// treating it as a separate failure mode.
func (p *pzProcess) Send(command string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("pz stdin already closed")
	}
	_, err := io.WriteString(p.stdin, command+"\n")
	return err
}

// Running reports whether the process has not yet been reaped.
func (p *pzProcess) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Wait blocks until the process has been reaped or the timeout expires.
func (p *pzProcess) Wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

// Stop ends the process, escalating only as far as it has to: the configured
// quit command, then a signal to the whole process group, then a kill.
//
// The group matters. PZ is launched through a shell script that execs java, and
// v1 sent SIGTERM to the script's pid — which the shell does not forward, so the
// JVM kept running and the halt relied on the container being torn down.
func (p *pzProcess) Stop(logf func(string, ...any)) {
	if !p.Running() {
		return
	}
	if p.cfg.QuitCommand != "" {
		logf("pz: sending %q, waiting up to %v", p.cfg.QuitCommand, p.cfg.QuitTimeout.D())
		if err := p.Send(p.cfg.QuitCommand); err != nil {
			logf("pz: could not write quit: %v", err)
		} else if p.Wait(p.cfg.QuitTimeout.D()) {
			logf("pz: exited on request")
			p.closeStdin()
			return
		}
	}
	logf("pz: still running after %v; terminating the process group", p.cfg.QuitTimeout.D())
	if err := terminate(p.cmd); err != nil {
		logf("pz: terminate: %v", err)
	}
	// A JVM that ignores SIGTERM is not going to become more cooperative; a
	// third of the quit budget is enough to notice it flushing and exiting.
	if p.Wait(p.cfg.QuitTimeout.D() / 3) {
		p.closeStdin()
		return
	}
	logf("pz: killing the process group")
	if err := killTree(p.cmd); err != nil {
		logf("pz: kill: %v", err)
	}
	p.Wait(30 * time.Second)
	p.closeStdin()
}

func (p *pzProcess) closeStdin() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		p.stdin.Close()
	}
}

// Player-count recognisers, in the order they are tried. These are ported from
// v1's parse_player_count with one change that is the actual fix for bug 1: a
// line nobody recognises produces no answer at all, where v1 returned 0.
//
// Returning 0 for "I could not tell" is what made the dashboard claim an empty
// server. The count is only ever written from a line that really carried one.
var (
	playersCountRe  = regexp.MustCompile(`(?i)players\s+connected\s*\(\s*(\d+)\s*\)`)
	playersNumberRe = regexp.MustCompile(`(?i)(\d+)\s+players?\s+connected`)
	playersNoneRe   = regexp.MustCompile(`(?i)no\s+players\s+connected`)
)

// parsePlayers reports a player count if this line carries one.
func parsePlayers(line string) (int, bool) {
	s := strings.TrimSpace(line)
	if m := playersCountRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		return n, err == nil
	}
	if m := playersNumberRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		return n, err == nil
	}
	if playersNoneRe.MatchString(s) {
		return 0, true
	}
	return 0, false
}

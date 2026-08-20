package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// A fake Project Zomboid.
//
// startPZ inherits os.Environ(), so the test binary can be its own fake server:
// TestMain notices PZ_FAKE_PZ and runs the fake instead of the tests. That gives
// the suite a real child process with real pipes and a real exit — the three
// things v1's log-tailing design could never test — without shipping a second
// binary or asking the workstation for a copy of the game.
const (
	fakePZEnv       = "PZ_FAKE_PZ"
	fakePlayersEnv  = "PZ_FAKE_PLAYERS"
	fakeNoSaveEnv   = "PZ_FAKE_NO_SAVE_CONFIRM"
	fakeIgnoreQuit  = "PZ_FAKE_IGNORE_QUIT"
	fakeExitAfter   = "PZ_FAKE_EXIT_AFTER"
	fakeExitCode    = "PZ_FAKE_EXIT_CODE"
	fakeBootDelay   = "PZ_FAKE_BOOT_DELAY"
	fakeSaveDelay   = "PZ_FAKE_SAVE_DELAY"
	fakePlayersFile = "PZ_FAKE_PLAYERS_FILE"
	// fakeTouchEnv names a file the fake appends its pid to on every launch. It is
	// how a test proves a negative — that the agent did not start the game — which
	// is the whole of the bug 2 fix.
	fakeTouchEnv = "PZ_FAKE_TOUCH"
)

const testBanner = "*** SERVER STARTED ***"

func TestMain(m *testing.M) {
	// Deliberately before flag.Parse: the fake is launched with PZ's command line
	// (-servername, -cachedir=…), which the testing flag set would reject.
	if os.Getenv(fakePZEnv) == "1" {
		fakePZMain()
		return
	}
	os.Exit(m.Run())
}

// fakePZMain speaks just enough of the console protocol for the agent to drive
// it: a ready banner, save, players, quit.
func fakePZMain() {
	if path := os.Getenv(fakeTouchEnv); path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
		}
	}
	if d, err := time.ParseDuration(os.Getenv(fakeBootDelay)); err == nil && d > 0 {
		time.Sleep(d)
	}
	if d, err := time.ParseDuration(os.Getenv(fakeExitAfter)); err == nil && d > 0 {
		go func() {
			time.Sleep(d)
			code, _ := strconv.Atoi(os.Getenv(fakeExitCode))
			os.Exit(code)
		}()
	}

	out := bufio.NewWriter(os.Stdout)
	say := func(f string, a ...any) {
		fmt.Fprintf(out, f+"\n", a...)
		out.Flush()
	}
	// Some noise first, so a test that sees "online" has really matched the banner
	// and not merely the first line of output.
	say("LOG  : General, 1700000000000> Loading world")
	say("znet: Java_zombie_core_znet_SteamUtils_init")
	say(testBanner)

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		switch strings.TrimSpace(sc.Text()) {
		case "save":
			if d, err := time.ParseDuration(os.Getenv(fakeSaveDelay)); err == nil && d > 0 {
				time.Sleep(d)
			}
			if os.Getenv(fakeNoSaveEnv) == "1" {
				say("LOG  : General, 1700000000001> saving...")
				continue
			}
			say("SAVED")
		case "players":
			say(fakePlayerLine())
		case "quit":
			if os.Getenv(fakeIgnoreQuit) == "1" {
				say("LOG  : General, 1700000000002> ignoring quit")
				continue
			}
			say("QUIT")
			out.Flush()
			os.Exit(0)
		}
	}
	// stdin closed: the parent is gone or has given up on us.
	os.Exit(0)
}

// fakePlayerLine formats a count in PZ's own wording. A file is consulted first so
// a test can change the count while the fake is running.
func fakePlayerLine() string {
	n := 0
	if path := os.Getenv(fakePlayersFile); path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			body := strings.TrimSpace(string(b))
			// "silent" makes the fake answer with a line no parser recognises,
			// which is how a test reaches the "report unknown, never zero" path.
			if body == "silent" {
				return "LOG  : General, 1700000000003> player list unavailable"
			}
			n, _ = strconv.Atoi(body)
		}
	} else {
		n, _ = strconv.Atoi(os.Getenv(fakePlayersEnv))
	}
	if n <= 0 {
		return "No players connected"
	}
	return fmt.Sprintf("Players connected (%d):", n)
}

// fakePZConfig is the process config the tests launch the fake with. The timeouts
// are short because every one of them is a real wait in the test.
func fakePZConfig() config.AgentPZ {
	c := config.Defaults().Agent.PZ
	c.ReadyBanner = testBanner
	c.SaveTimeout = config.Duration(3 * time.Second)
	c.QuitTimeout = config.Duration(3 * time.Second)
	c.PlayersInterval = config.Duration(200 * time.Millisecond)
	return c
}

// startFakePZ launches the test binary as a PZ server.
func startFakePZ(t *testing.T, cfg config.AgentPZ, logPath string) (*pzProcess, chan event) {
	t.Helper()
	t.Setenv(fakePZEnv, "1")

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan event, 64)
	pz, err := startPZ(context.Background(), self, nil, cfg, logPath, events, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pz.Running() {
			killTree(pz.cmd)
			pz.Wait(5 * time.Second)
		}
	})
	return pz, events
}

// waitEvent reads events until one of the wanted kind arrives.
func waitEvent(t *testing.T, events <-chan event, kind eventKind, within time.Duration) event {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev := <-events:
			if ev.kind == kind {
				return ev
			}
			t.Logf("skipping event kind %d while waiting for %d", ev.kind, kind)
		case <-deadline:
			t.Fatalf("no event of kind %d within %v", kind, within)
		}
	}
}

func TestStartPZReportsOnlinePlayersAndExit(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "server.log")
	t.Setenv(fakePlayersEnv, "3")
	pz, events := startFakePZ(t, fakePZConfig(), logPath)

	waitEvent(t, events, evOnline, 10*time.Second)

	if err := pz.Send("players"); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, events, evPlayers, 5*time.Second)
	if ev.players != 3 {
		t.Errorf("players = %d, want 3", ev.players)
	}

	// A save must be observable, because a backup that cannot tell whether the
	// world was flushed is the v1 behaviour this replaces.
	if err := pz.Send("save"); err != nil {
		t.Fatal(err)
	}
	if line, ok := pz.waitFor([]string{"SAVED"}, 5*time.Second); !ok {
		t.Error("no save confirmation")
	} else if !strings.Contains(line, "SAVED") {
		t.Errorf("confirmation line = %q", line)
	}

	pz.Stop(t.Logf)
	ev = waitEvent(t, events, evExit, 10*time.Second)
	if ev.code != 0 {
		t.Errorf("exit code = %d, want 0 for a requested quit", ev.code)
	}
	if pz.Running() {
		t.Error("Running() is still true after the exit event")
	}

	// The mirrored log is what an operator reads after the fact, and what the
	// dashboard tails. It must hold the game's own lines.
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), testBanner) {
		t.Errorf("the mirrored log does not contain the banner:\n%s", body)
	}
}

func TestStopEscalatesWhenQuitIsIgnored(t *testing.T) {
	t.Setenv(fakeIgnoreQuit, "1")
	cfg := fakePZConfig()
	cfg.QuitTimeout = config.Duration(700 * time.Millisecond)
	pz, events := startFakePZ(t, cfg, "")
	waitEvent(t, events, evOnline, 10*time.Second)

	start := time.Now()
	pz.Stop(t.Logf)
	// The escalation is the point: a JVM that ignores the console command is still
	// gone when Stop returns. v1 sent SIGTERM to the wrapper script instead, which
	// left the game running and made the halt depend on the container's teardown.
	if pz.Running() {
		t.Fatalf("the process survived Stop after %v", time.Since(start))
	}
	waitEvent(t, events, evExit, 10*time.Second)
}

func TestWaitForGivesUpWhenTheProcessExits(t *testing.T) {
	t.Setenv(fakeNoSaveEnv, "1")
	pz, events := startFakePZ(t, fakePZConfig(), "")
	waitEvent(t, events, evOnline, 10*time.Second)

	pz.Stop(t.Logf)
	// A five-minute save_timeout must not become a five-minute stall once there is
	// no process left to answer: waitFor watches the exit too.
	start := time.Now()
	if _, ok := pz.waitFor([]string{"SAVED"}, time.Minute); ok {
		t.Error("waitFor reported a match from a dead process")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("waitFor blocked for %v after the process had exited", elapsed)
	}
}

func TestSendFailsAfterStop(t *testing.T) {
	pz, events := startFakePZ(t, fakePZConfig(), "")
	waitEvent(t, events, evOnline, 10*time.Second)
	pz.Stop(t.Logf)
	if err := pz.Send("save"); err == nil {
		t.Error("Send succeeded after Stop closed stdin")
	}
}

func TestParsePlayersNeverInventsAZero(t *testing.T) {
	for _, tc := range []struct {
		line string
		n    int
		ok   bool
	}{
		// Recognised, in the wordings PZ actually uses.
		{"Players connected (4):", 4, true},
		{"players connected (0):", 0, true},
		{"Players connected ( 12 ):", 12, true},
		{"4 players connected", 4, true},
		{"1 player connected", 1, true},
		{"No players connected", 0, true},
		{"  no players connected  ", 0, true},

		// Not a count. Every one of these returned 0 in v1's parser, and that is
		// the whole of bug 1: the dashboard showed an empty server because the
		// controller wrote a fabricated zero over a count it never measured.
		{"", 0, false},
		{"LOG  : General, 1700000000000> Loading world", 0, false},
		{"znet: connection established", 0, false},
		{"players", 0, false},
		{"Player connected: hrkcz001", 0, false},
		{"MaxPlayers=32", 0, false},
		{"java.lang.NullPointerException", 0, false},
		{testBanner, 0, false},
	} {
		n, ok := parsePlayers(tc.line)
		if n != tc.n || ok != tc.ok {
			t.Errorf("parsePlayers(%q) = (%d, %v), want (%d, %v)", tc.line, n, ok, tc.n, tc.ok)
		}
	}
}

func TestFindLauncherHonoursTheConfiguredOrder(t *testing.T) {
	dir := t.TempDir()
	// Both present, in different subtrees. v1 used `find -name a -o -name b |
	// head -1`, so which launcher ran depended on the order the filesystem
	// happened to yield — the same image could boot differently on two providers.
	writeFile(t, filepath.Join(dir, "z-sub", "StartServer64.sh"), "#!/bin/sh\n")
	writeFile(t, filepath.Join(dir, "a-sub", "start-server.sh"), "#!/bin/sh\n")

	got, err := findLauncher(dir, []string{"start-server.sh", "StartServer64.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "start-server.sh" {
		t.Errorf("launcher = %s, want the first configured name", got)
	}

	got, err = findLauncher(dir, []string{"StartServer64.sh", "start-server.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "StartServer64.sh" {
		t.Errorf("launcher = %s, want the first configured name after the order changed", got)
	}
}

func TestFindLauncherReportsWhatItLookedFor(t *testing.T) {
	_, err := findLauncher(t.TempDir(), []string{"start-server.sh", "StartServer64.sh"})
	if err == nil {
		t.Fatal("findLauncher succeeded in an empty directory")
	}
	// The message is the operator's only clue when an image changes layout.
	if !strings.Contains(err.Error(), "start-server.sh") || !strings.Contains(err.Error(), "StartServer64.sh") {
		t.Errorf("error = %q, want both candidate names in it", err)
	}
}

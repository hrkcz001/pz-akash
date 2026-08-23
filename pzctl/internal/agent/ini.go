package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// Rendering PZ's own configuration files.
//
// The division of labour is deliberate and is what keeps secrets out of git: the
// .ini arrives inside server.zip with the passwords already substituted by the
// controller, and the agent overwrites only the keys that config.yaml owns. Keys
// it does not own — RCONPassword, Password, every sandbox-adjacent setting an
// operator hand-edited — are preserved byte for byte.
//
// v1 had no such split. The .ini was committed to git with the passwords in it,
// and the game values were whatever the file happened to contain, so changing
// MaxPlayers meant editing a file in a repository and hoping the right copy won.

// ownedINI is the exact set of .ini keys config.yaml controls. Anything absent
// from this map is left alone.
func ownedINI(c *config.Config) map[string]string {
	g := c.Game
	vals := map[string]string{
		"Map":                    g.Map,
		"PublicName":             g.PublicName,
		"MaxPlayers":             strconv.Itoa(g.MaxPlayers),
		"PauseEmpty":             boolINI(g.PauseEmpty),
		"Public":                 boolINI(g.Public),
		"Open":                   boolINI(g.Open),
		"GlobalChat":             boolINI(g.GlobalChat),
		"PingLimit":              strconv.Itoa(g.PingLimit),
		"MaxAccountsPerUser":     strconv.Itoa(g.MaxAccountsPerUser),
		"UPnP":                   boolINI(g.UPnP),
		"SaveWorldEveryMinutes":  strconv.Itoa(g.SaveWorldEveryMinutes),
		"BackupsCount":           strconv.Itoa(g.PZBackups.Count),
		"BackupsOnStart":         boolINI(g.PZBackups.OnStart),
		"BackupsOnVersionChange": boolINI(g.PZBackups.OnVersionChange),
		"BackupsPeriod":          strconv.Itoa(g.PZBackups.Period),
		"DefaultPort":            strconv.Itoa(c.Server.Ports.Game),
	}
	// UDPPort is written only when a second socket is configured. server.ports.udp:
	// 0 means "PZ binds one", and the key is then left as the file has it rather
	// than set to 0 — 0 is not a port, and PZ would either reject it or bind
	// something arbitrary. Nothing exposes it in the SDL either, so an unwritten
	// key and an unexposed port stay consistent.
	if c.Server.Ports.UDP > 0 {
		vals["UDPPort"] = strconv.Itoa(c.Server.Ports.UDP)
	}
	// RCONPort is only written when RCON is on. PZ has no RCONEnabled key — it
	// listens when there is a password — so writing the port while the feature is
	// off would open a port the SDL does not even expose.
	if c.Server.RCON.Enabled {
		vals["RCONPort"] = strconv.Itoa(c.Server.RCON.Port)
	}
	return vals
}

func boolINI(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// renderServerINI rewrites path so the owned keys carry the configured values,
// preserving every other line, its order, and its comments. Keys that are absent
// from the file are appended.
//
// It reports which keys it changed, which is what the boot log shows: "config
// drift" is otherwise invisible until someone wonders why MaxPlayers is 16.
func renderServerINI(path string, c *config.Config) ([]string, error) {
	want := ownedINI(c)

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	var out []string
	var changed []string
	seen := map[string]bool{}

	sc := bufio.NewScanner(strings.NewReader(string(existing)))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		key, _, isPair := splitINI(line)
		if !isPair {
			out = append(out, line)
			continue
		}
		v, owned := want[key]
		if !owned {
			out = append(out, line)
			continue
		}
		seen[key] = true
		replacement := key + "=" + v
		if replacement != line {
			changed = append(changed, key)
		}
		out = append(out, replacement)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// Append whatever the file did not already have, sorted so a diff of two
	// boots is empty rather than reordered.
	var missing []string
	for k := range want {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		out = append(out, k+"="+want[k])
		changed = append(changed, k)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	body := strings.Join(out, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if err := writeFileAtomic(path, []byte(body), 0o644); err != nil {
		return nil, err
	}
	sort.Strings(changed)
	return changed, nil
}

// splitINI recognises a `key=value` line. Comment markers (# and ;) and blank
// lines are not pairs, so they survive untouched.
func splitINI(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
		return "", "", false
	}
	i := strings.Index(t, "=")
	if i <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(t[:i]), strings.TrimSpace(t[i+1:]), true
}

// readINIKey returns the value of one key, or "" when the file has no such key.
//
// A placeholder value counts as absent. The controller substitutes secrets into
// the .ini as it serves server.zip, and when it has no value for one the token is
// still in the file; treating `__ADMIN_PASSWORD__` as a password would set the
// admin account to that literal string. Same `__*__` rule the image gate uses.
func readINIKey(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		k, v, ok := splitINI(sc.Text())
		if !ok || k != key {
			continue
		}
		if strings.HasPrefix(v, "__") && strings.HasSuffix(v, "__") && len(v) > 4 {
			return "", nil
		}
		return v, nil
	}
	return "", sc.Err()
}

// launchJSON is the part of ProjectZomboid64.json the agent touches. The file has
// other keys (classpath, mainClass) that must survive, so it is decoded into a
// generic map rather than a struct.
const (
	steamOn  = "-Dzomboid.steam=1"
	steamOff = "-Dzomboid.steam=0"
)

// patchLaunchJSON sets the JVM heap flags and forces Steam off.
//
// The heap flags have to go here rather than on the launcher's command line: the
// pzexe launcher does not forward unknown CLI options to the JVM, it logs
// "unknown option" and drops them — which is how v1's first attempt at 14 GiB
// ended up running on the JSON's default heap. That discovery is recorded in the
// v1 entrypoint and is preserved here.
func patchLaunchJSON(path, xmx, xms string) (changed bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}

	var args []string
	if existing, ok := doc["vmArgs"].([]any); ok {
		for _, a := range existing {
			s, ok := a.(string)
			if !ok {
				continue
			}
			switch {
			case strings.HasPrefix(s, "-Xmx"), strings.HasPrefix(s, "-Xms"):
				continue // replaced below
			case s == steamOn:
				s = steamOff
			}
			args = append(args, s)
		}
	}
	if !containsStr(args, steamOff) {
		args = append(args, steamOff)
	}
	if xmx != "" {
		args = append(args, "-Xmx"+xmx)
	}
	if xms != "" {
		args = append(args, "-Xms"+xms)
	}
	doc["vmArgs"] = args

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if string(out) == string(raw) {
		return false, nil
	}
	return true, writeFileAtomic(path, out, 0o644)
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// writeFileAtomic writes through a temporary file in the same directory. The
// game reads these files at startup and an operator may be reading them over a
// shell; a half-written .ini is worse than an old one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Fetcher reads one file from a source — a directory, or a git ref. An error
// satisfying fs.ErrNotExist means the file is absent, which for most of the v1
// state files is meaningful rather than exceptional.
type Fetcher func(path string) ([]byte, error)

// DirFetcher reads from a directory on disk.
func DirFetcher(dir string) Fetcher {
	return func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
	}
}

func fetchText(f Fetcher, path string) (string, bool) {
	b, err := f(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func exists(f Fetcher, path string) bool {
	_, err := f(path)
	return err == nil
}

// LegacyServerInfo mirrors v1's server_info.json. It exists only so the v1
// document can be decoded with the same repair-on-read machinery as everything
// else — which is the point, since the live copy of this file is malformed.
type LegacyServerInfo struct {
	IP           string  `json:"ip"`
	Port         int     `json:"port"` // sshd, which v2 does not run
	GamePort     int     `json:"game_port"`
	Status       string  `json:"status"`
	PlayersCount int     `json:"players_count"`
	PricePerHour float64 `json:"price_per_hour"`
	PricePerDay  float64 `json:"price_per_day"`
}

// LegacyControllerInfo mirrors v1's controller_info.json.
type LegacyControllerInfo struct {
	StorageURL    string `json:"storage_url"`
	RawStorageURL string `json:"raw_storage_url"`
	WebhookURL    string `json:"webhook_url"`
	UpdatedAt     Stamp  `json:"updated_at"`
}

// LegacyTriggers is which v1 trigger files were present at import time.
type LegacyTriggers struct {
	Start  bool
	Backup bool
	Halt   bool
	StopAt string
}

// mergePrefixed folds a sub-document's repairs into r, qualifying each field
// with the file it came from. A whole-document failure carries no field name, so
// the file name stands alone rather than being reported as "file:" with nothing
// after the colon — which is precisely the case the live server_info.json hits.
func mergePrefixed(r *Repairs, file string, sub *Repairs) {
	for _, it := range sub.Items {
		if it.Field == "" {
			it.Field = file
		} else {
			it.Field = file + ":" + it.Field
		}
		r.Items = append(r.Items, it)
	}
}

// ImportLegacy reads the v1 file set and produces the v2 documents.
//
// It is deliberately conservative about two things. Player count is discarded
// rather than carried over, because every v1 write site hardcoded zero and
// importing that would launder a known-false value into a document that claims
// to be measured. And an unreadable status with a live dseq becomes
// StatusFailed, not StatusOffline: "we do not know, and something is billing" is
// the honest reading, and it forces reconciliation before anything else happens.
func ImportLegacy(f Fetcher, loc *time.Location) (*Controller, *Backups, *Repairs) {
	r := &Repairs{}
	c := NewController(loc)
	idx := NewBackups()

	// --- server_info.json ---
	var si LegacyServerInfo
	si.PlayersCount = PlayersUnknown
	if raw, err := f("server_info.json"); err == nil {
		mergePrefixed(r, "server_info.json", Unmarshal(raw, &si))
	} else if !errors.Is(err, fs.ErrNotExist) {
		r.add("server_info.json", "unreadable: %v", err)
	}

	c.Endpoint = Endpoint{GamePort: si.GamePort}
	if si.IP != "" && si.IP != "pending" {
		// "pending" was v1's in-band sentinel for "no address yet". v2 uses an
		// empty IP and Endpoint.Ready() instead.
		c.Endpoint.IP = si.IP
	} else if si.IP == "pending" {
		r.add("server_info.json:ip", `dropped v1 sentinel "pending"`)
	}
	if si.Port != 0 {
		r.add("server_info.json:port", "was the sshd port (%d); v2 runs no sshd, dropped", si.Port)
	}
	c.Price = Price{USDPerHour: si.PricePerHour, USDPerDay: si.PricePerDay}
	if si.PricePerHour > 0 || si.PricePerDay > 0 {
		c.Price.QuotedAt = Now(loc)
	}
	r.add("server_info.json:players_count",
		"discarded: every v1 write site hardcoded 0, so the value was never a measurement")

	// --- active_dseq ---
	if dseq, ok := fetchText(f, "active_dseq"); ok && dseq != "" {
		c.Lease = &Lease{DSeq: dseq}
	}

	// --- status ---
	status, statusRepair := importStatus(si.Status, c.Lease != nil)
	c.Status = status
	if statusRepair != "" {
		r.add("server_info.json:status", "%s", statusRepair)
	}

	// --- desired_state / halt ---
	tr := LegacyTriggers{
		Start:  exists(f, "start"),
		Backup: exists(f, "backup"),
		Halt:   exists(f, "halt"),
	}
	if s, ok := fetchText(f, "stop_at"); ok {
		tr.StopAt = s
	}

	c.Intent = IntentStopped
	if ds, ok := fetchText(f, "desired_state"); ok {
		switch Intent(ds) {
		case IntentRunning, IntentStopped:
			c.Intent = Intent(ds)
		default:
			r.add("desired_state", "unrecognised value %q, treated as stopped", ds)
		}
	}
	if tr.Halt && c.Intent != IntentStopped {
		c.Intent = IntentStopped
		r.add("halt", "trigger present, so intent is stopped regardless of desired_state")
	}
	// Unconsumed triggers are reported, never acted on. Importing is a read;
	// deciding to deploy because a stale `start` file was lying around is how a
	// cutover turns into a surprise lease.
	for _, t := range []struct {
		name    string
		present bool
	}{{"start", tr.Start}, {"backup", tr.Backup}, {"halt", tr.Halt}} {
		if t.present {
			r.add(t.name, "unconsumed v1 trigger file; move it to triggers/ and push to act on it")
		}
	}

	if tr.StopAt != "" {
		if when, err := ParseStopAt(tr.StopAt, loc); err != nil {
			r.add("stop_at", "unparseable (%v), dropped", err)
		} else {
			s := At(when)
			c.StopAt = &s
		}
	}

	// --- controller_info.json ---
	var ci LegacyControllerInfo
	if raw, err := f("controller_info.json"); err == nil {
		mergePrefixed(r, "controller_info.json", Unmarshal(raw, &ci))
		c.URLs = URLs{Public: ci.StorageURL, Raw: ci.RawStorageURL, Webhook: ci.WebhookURL}
	} else if !errors.Is(err, fs.ErrNotExist) {
		r.add("controller_info.json", "unreadable: %v", err)
	}

	// --- backup_log -> index ---
	if log, ok := fetchText(f, "backup_log"); ok {
		for _, line := range strings.Split(log, "\n") {
			name := strings.TrimSpace(line)
			if name == "" {
				continue
			}
			if !IsBackupName(name) {
				r.add("backup_log", "ignored non-backup entry %q", name)
				continue
			}
			e := Backup{Name: name}
			// v1 recorded no size or checksum, so the instant has to come from
			// the filename. It was written by a UTC container clock, which is
			// exactly the inconsistency identity.timezone now removes.
			if when, err := ParseBackupName(name, time.UTC); err == nil {
				e.CreatedAt = At(when.In(loc))
			}
			idx.Upsert(e)
		}
		idx.UpdatedAt = Now(loc)
		if len(idx.Items) > 0 {
			r.add("backup_log",
				"imported %d name(s) with no size or checksum; rebuild the index from disk to fill those in",
				len(idx.Items))
		}
	}

	// --- restore_target / request_restore ---
	target, hasTarget := fetchText(f, "restore_target")
	req, hasReq := fetchText(f, "request_restore")
	switch {
	case !hasTarget || target == "":
		if hasReq && req != "" {
			r.add("request_restore", "restore requested but restore_target is empty, dropped")
		}
	case !hasReq:
		r.add("restore_target",
			"%q kept as bookkeeping only: no request_restore flag, so nothing was pending", target)
	case !idx.Has(target):
		// This is the live drift: restore_target named backup_20260819_013704.zip
		// while backup_log listed only ...013623.zip and ...003520.zip.
		r.add("restore_target",
			"%q is not in backup_log, so the file it names may not exist; dropped rather than failing a boot", target)
	default:
		c.RestoreTarget = target
	}

	c.UpdatedAt = Now(loc)
	c.Since = Now(loc)
	// Both documents go through the same normalization as a decoded one, so an
	// import cannot produce a shape a read would have rejected.
	c.Normalize(r)
	idx.Normalize(r)
	return c, idx, r
}

// importStatus maps a v1 status string onto a v2 Status.
func importStatus(v1 string, leased bool) (Status, string) {
	switch strings.ToLower(strings.TrimSpace(v1)) {
	case "offline", "stopped":
		return StatusOffline, ""
	case "booting":
		return StatusBooting, ""
	case "online":
		return StatusOnline, ""
	case "stopping":
		return StatusStopping, ""
	case "":
		if leased {
			return StatusFailed, "missing, and a dseq is active — needs reconciliation against Akash"
		}
		return StatusOffline, "missing, and no dseq is active — assumed offline"
	default:
		if leased {
			return StatusFailed, "unrecognised value " + v1 + ", and a dseq is active — needs reconciliation"
		}
		return StatusOffline, "unrecognised value " + v1 + ", assumed offline"
	}
}

// stopAtLayouts are the formats v1's Python helper accepted, in the same order.
var stopAtLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
}

// ParseStopAt reads a scheduled-stop timestamp. RFC 3339 with an offset is
// preferred; a bare local form is interpreted in loc.
//
// v1 parsed the bare forms and then stamped them UTC unconditionally, so
// "stop at 23:00" meant 01:00 the next morning in Prague. Interpreting them in
// the configured location is the fix, and an explicit offset always wins.
func ParseStopAt(s string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty")
	}
	when, err := parseStopAt(s, loc)
	if err != nil {
		return time.Time{}, err
	}
	// The zero time parses cleanly from "0001-01-01 0:00" and from a Unix 0, and
	// it is in the past forever — a stop that fires on every tick, immediately
	// after every boot. Callers cannot tell it apart from "no stop scheduled", so
	// it is rejected here rather than dropped three layers later.
	if when.IsZero() || when.Year() < 2000 {
		return time.Time{}, errors.New("not a plausible stop time")
	}
	return when, nil
}

func parseStopAt(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(loc), nil
	}
	// Unix seconds, which v1 also accepted.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).In(loc), nil
	}
	for _, layout := range stopAtLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("want RFC 3339, `YYYY-MM-DD HH:MM[:SS]`, or Unix seconds")
}

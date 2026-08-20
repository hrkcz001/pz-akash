package state

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	// Embedded so time.LoadLocation works in a scratch container and on Windows,
	// neither of which ships the zoneinfo database.
	_ "time/tzdata"
)

// Stamp is a timestamp that serialises as RFC 3339 with an explicit offset.
//
// Two properties matter. First, the offset means the value is an unambiguous
// instant, so retention and ordering are correct even across a DST boundary.
// Second, it renders in whatever location the time.Time carries, so a state
// branch diff reads in the operator's own timezone instead of forcing them to
// convert UTC in their head.
//
// Unmarshalling also accepts a bare integer for compatibility with the v1
// documents, which stored Unix seconds.
type Stamp struct{ time.Time }

// Now returns the current instant in loc. A nil loc means UTC.
func Now(loc *time.Location) Stamp {
	if loc == nil {
		loc = time.UTC
	}
	return Stamp{time.Now().In(loc)}
}

// At wraps t unchanged.
func At(t time.Time) Stamp { return Stamp{t} }

// Zero reports whether the stamp was never set.
func (s Stamp) Zero() bool { return s.Time.IsZero() }

// In returns the same instant rendered in loc.
func (s Stamp) In(loc *time.Location) Stamp {
	if loc == nil || s.Zero() {
		return s
	}
	return Stamp{s.Time.In(loc)}
}

// Age is how long ago the stamp was, measured from now. A zero stamp reports a
// duration large enough that any freshness check treats it as stale, so a
// missing timestamp can never read as "just updated".
func (s Stamp) Age() time.Duration {
	if s.Zero() {
		return time.Duration(1 << 62)
	}
	return time.Since(s.Time)
}

func (s Stamp) MarshalJSON() ([]byte, error) {
	if s.Zero() {
		return []byte(`null`), nil
	}
	return json.Marshal(s.Time.Format(time.RFC3339))
}

func (s *Stamp) UnmarshalJSON(b []byte) error {
	txt := strings.TrimSpace(string(b))
	if txt == "null" || txt == `""` {
		s.Time = time.Time{}
		return nil
	}
	// v1 wrote Unix seconds, e.g. "updated_at": 1787099745.
	if n, err := strconv.ParseInt(txt, 10, 64); err == nil {
		s.Time = time.Unix(n, 0).UTC()
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return fmt.Errorf("timestamp is neither a string nor Unix seconds: %s", txt)
	}
	str = strings.TrimSpace(str)
	if str == "" {
		s.Time = time.Time{}
		return nil
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return fmt.Errorf("timestamp %q is not RFC 3339: %w", str, err)
	}
	s.Time = t
	return nil
}

func (s Stamp) String() string {
	if s.Zero() {
		return "never"
	}
	return s.Time.Format(time.RFC3339)
}

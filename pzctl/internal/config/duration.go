package config

import (
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that YAML accepts in either form:
//
//	interval: 1h30m     # Go duration string
//	interval: 5400      # bare number of seconds
//
// The second form exists so that every `*_SEC` value from the old bash system
// can be pasted in verbatim during migration. It always marshals back out as a
// duration string.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Seconds returns the value in whole seconds, truncating.
func (d Duration) Seconds() int { return int(time.Duration(d).Seconds()) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	switch n.Tag {
	case "!!str":
		v, err := time.ParseDuration(n.Value)
		if err != nil {
			return fmt.Errorf("line %d: %q is not a duration (want e.g. 90s, 5m, 1h30m, or a plain number of seconds)", n.Line, n.Value)
		}
		*d = Duration(v)
	case "!!int", "!!float":
		f, err := strconv.ParseFloat(n.Value, 64)
		if err != nil {
			return fmt.Errorf("line %d: %q is not a number of seconds", n.Line, n.Value)
		}
		*d = Duration(time.Duration(f * float64(time.Second)))
	default:
		return fmt.Errorf("line %d: expected a duration string or a number of seconds, got %s", n.Line, n.Tag)
	}
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

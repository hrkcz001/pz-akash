package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
)

// Repair is one thing that was wrong with a document on disk. Reading never
// fails on a repairable problem; it reports what it had to fix so the caller can
// log it and, if the value mattered, reconcile from an authoritative source.
type Repair struct {
	// Field is the JSON path, or "" for a whole-document problem.
	Field string
	// Detail is what was wrong, in operator-readable terms.
	Detail string
	// Fatal marks a document that could not be parsed at all, so every field is
	// now a default rather than a stored value. A caller holding money-relevant
	// state (a lease) must treat this as "reconcile before acting", never as
	// "there is no lease".
	Fatal bool
}

func (r Repair) String() string {
	if r.Field == "" {
		return r.Detail
	}
	return r.Field + ": " + r.Detail
}

// Repairs collects every problem found in one read.
type Repairs struct {
	Items []Repair
}

func (r *Repairs) add(field, format string, args ...any) {
	r.Items = append(r.Items, Repair{Field: field, Detail: fmt.Sprintf(format, args...)})
}

func (r *Repairs) addFatal(format string, args ...any) {
	r.Items = append(r.Items, Repair{Detail: fmt.Sprintf(format, args...), Fatal: true})
}

// OK reports whether the document was exactly what we would have written.
func (r *Repairs) OK() bool { return r == nil || len(r.Items) == 0 }

// Fatal reports whether the document was unparseable and the result is entirely
// defaults.
func (r *Repairs) Fatal() bool {
	if r == nil {
		return false
	}
	for _, it := range r.Items {
		if it.Fatal {
			return true
		}
	}
	return false
}

func (r *Repairs) String() string {
	if r.OK() {
		return "no repairs"
	}
	parts := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		parts = append(parts, it.String())
	}
	return strings.Join(parts, "; ")
}

var unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// Unmarshal decodes data into dst, salvaging every field it can, then calls
// dst.Normalize if dst is a Normalizer.
//
// This is the repair-on-read half of invariant I2, and it is written this way
// because of what v1 did with a single bad byte. Its state file grew
// `"price_per_hour": ,` from an unquoted empty shell variable; the controller's
// `jq -r '.status'` then returned empty, the status check failed, and periodic
// backups and restore handling both silently stopped. One malformed field
// disabled two unrelated features. Here a malformed field costs exactly that
// field: it is reset to the value dst already held (the caller's default) and
// reported.
//
// Normalize runs on every return path, including the fatal ones, so the
// postcondition holds unconditionally: after Unmarshal, dst satisfies its own
// invariants or the Repairs say why it could not.
//
// dst must be a non-nil pointer to a struct, pre-populated with defaults.
func Unmarshal(data []byte, dst any) *Repairs {
	r := &Repairs{}
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		r.addFatal("internal: Unmarshal wants a non-nil pointer to a struct, got %T", dst)
		return r
	}
	defer normalize(dst, r)

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		r.addFatal("document is empty")
		return r
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		// This is the v1 corruption case. Note the byte offset in the message:
		// it is what makes "Expecting value: line 1 column 106" actionable
		// instead of merely alarming.
		r.addFatal("not a JSON object (%v); every field fell back to its default", err)
		return r
	}
	decodeStruct(obj, v.Elem(), "", r)
	return r
}

func normalize(dst any, r *Repairs) {
	if n, ok := dst.(Normalizer); ok {
		n.Normalize(r)
	}
}

// decodeStruct fills one struct level from a raw object, recursing into nested
// structs so a bad leaf does not discard its siblings.
//
// Fields are visited in declaration order and unknown keys in sorted order, so
// the repair list is deterministic — a log line that reorders itself between
// runs is one nobody can diff.
func decodeStruct(obj map[string]json.RawMessage, dst reflect.Value, prefix string, r *Repairs) {
	typ := dst.Type()
	type fieldRef struct {
		name string
		idx  int
	}
	fields := make([]fieldRef, 0, typ.NumField())
	known := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields = append(fields, fieldRef{name: name, idx: i})
		known[name] = true
	}

	unknown := make([]string, 0)
	for key := range obj {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		// Tolerated, not fatal: the live repo is full of v1 keys, and refusing
		// to start because of one would hand the operator a dead controller
		// instead of a slightly stale document.
		r.add(prefix+key, "unknown field, ignored")
	}

	for _, fr := range fields {
		raw, present := obj[fr.name]
		if !present {
			continue
		}
		field := dst.Field(fr.idx)
		path := prefix + fr.name
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			// An explicit null means "unset"; leave the default in place rather
			// than zeroing, so a null lease does not read as a zero-value lease.
			continue
		}
		if err := json.Unmarshal(raw, field.Addr().Interface()); err == nil {
			continue
		} else if !salvage(raw, field, path, r) {
			r.add(path, "%s; kept default %s", trimJSONError(err), render(field))
		}
	}
}

// salvage retries a failed field decode one level deeper. It returns true if it
// managed to recover at least the shape, in which case any bad leaves have
// already been reported.
func salvage(raw json.RawMessage, field reflect.Value, path string, r *Repairs) bool {
	// A type with its own UnmarshalJSON owns its parsing; going behind it would
	// produce values it never validated.
	if reflect.PointerTo(field.Type()).Implements(unmarshalerType) ||
		field.Type().Implements(unmarshalerType) {
		return false
	}

	target := field
	var commit func()
	if field.Kind() == reflect.Pointer {
		if field.Type().Elem().Kind() != reflect.Struct {
			return false
		}
		fresh := reflect.New(field.Type().Elem())
		if !field.IsNil() {
			// Start from the default the caller supplied, so an unsalvageable
			// leaf keeps its default rather than becoming a zero value.
			fresh.Elem().Set(field.Elem())
		}
		target = fresh.Elem()
		commit = func() { field.Set(fresh) }
	}
	if target.Kind() != reflect.Struct {
		return false
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return false
	}
	decodeStruct(nested, target, path+".", r)
	if commit != nil {
		commit()
	}
	return true
}

// trimJSONError shortens encoding/json's messages, which name Go types the
// operator has never heard of.
func trimJSONError(err error) string {
	msg := err.Error()
	if te, ok := err.(*json.UnmarshalTypeError); ok {
		return fmt.Sprintf("expected %s, got JSON %s", te.Type, te.Value)
	}
	return msg
}

func render(v reflect.Value) string {
	b, err := json.Marshal(v.Interface())
	if err != nil {
		return "<unprintable>"
	}
	if len(b) > 60 {
		return string(b[:57]) + "..."
	}
	return string(b)
}

// Marshal encodes a document for storage: indented, newline-terminated, and with
// HTML escaping off so a URL in the document stays readable in a git diff.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("marshal %T: %w", v, err)
	}
	return buf.Bytes(), nil
}

// WriteFile writes data to path atomically: a temp file in the same directory,
// fsynced, then renamed over the target.
//
// The rename is what matters. v1 wrote state with shell redirection, so a reader
// could observe a truncated file, and a crash mid-write left one permanently.
// After this call the file on disk is either entirely the old content or
// entirely the new.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", name, path, err)
	}
	syncDir(dir)
	return nil
}

// syncDir persists the rename itself. Best effort: Windows cannot fsync a
// directory handle opened this way, and on a container filesystem the call may
// be unsupported. A missed directory sync costs at most the most recent write
// after a hard power loss, which is a risk the design already accepts by keeping
// the durable copy in git.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// ReadFileInto loads path into dst, applying repair-on-read. A missing file is
// not an error: it means "never written", and dst keeps its defaults — still
// normalized, so the postcondition is the same on every path.
func ReadFileInto(path string, dst any) (*Repairs, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			r := &Repairs{}
			normalize(dst, r)
			return r, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Unmarshal(data, dst), nil
}

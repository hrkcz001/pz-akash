package dns

// Records: the read-compare-write that keeps one name pointing at one address.
//
// Two rules run through this file.
//
// The first is that only address records are ever touched. A zone's apex commonly
// carries MX and TXT records — mail routing, SPF, domain verification — and a
// "clear the name and write ours" that filtered by name alone would delete the
// operator's email. Filtering by type is what makes writing to the apex safe.
//
// The second is that an unchanged record is left alone. Cloudflare accepts a PUT
// that changes nothing, but these syncs run on every deploy, and a zone whose audit
// log records a write per redeploy hides the write that actually mattered.

import (
	"context"
	"net/url"
	"strings"
)

// addressTypes are the record types that answer "where is this host". Anything
// else at the same name belongs to somebody else and is never read or written.
var addressTypes = map[string]bool{"A": true, "AAAA": true, "CNAME": true}

// recordComment marks what wrote a record, so an operator looking at the zone can
// tell a managed record from one they added by hand. It is deliberately left out of
// the comparison below: an operator's own note on our record is not a reason to
// rewrite it.
const recordComment = "managed by pzctl"

// record is the subset of a Cloudflare DNS record this package reads and writes.
type record struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

// body is what gets sent. Separate from record so an ID read from the API cannot
// be echoed back into a create, and so `proxied: false` is always transmitted —
// omitting it would let Cloudflare default a game record to proxied, which is the
// one mistake in this package that would take the server off the map for every
// player at once.
func (r record) body() map[string]any {
	return map[string]any{
		"type":    r.Type,
		"name":    r.Name,
		"content": r.Content,
		"proxied": r.Proxied,
		"ttl":     r.TTL,
		"comment": recordComment,
	}
}

// same reports whether the live record already says what we want.
func (r record) same(want record) bool {
	return r.Type == want.Type &&
		strings.EqualFold(strings.TrimSuffix(r.Content, "."), strings.TrimSuffix(want.Content, ".")) &&
		r.Proxied == want.Proxied &&
		r.TTL == want.TTL
}

// list returns the address records at name.
func (c *Cloudflare) list(ctx context.Context, name string) ([]record, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("per_page", "100")
	var out []record
	if err := c.do(ctx, "GET", "/zones/"+c.zoneID+"/dns_records?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, r := range out {
		if addressTypes[strings.ToUpper(r.Type)] {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// upsert makes name resolve to want and nothing else.
//
// When several address records share the name — a leftover AAAA from a provider
// that had one, or a CNAME from before a dedicated IP — one is rewritten in place
// and the rest are deleted. Leaving them would mean half the players resolving to
// the old address, which is far harder to diagnose than no record at all.
func (c *Cloudflare) upsert(ctx context.Context, want record) (Change, error) {
	existing, err := c.list(ctx, want.Name)
	if err != nil {
		return Change{}, err
	}

	ch := Change{
		Name: want.Name, Type: want.Type, Content: want.Content,
		Proxied: want.Proxied, TTL: want.TTL,
	}

	// Prefer the record already of the right type: rewriting that one changes an
	// address, while rewriting a CNAME into an A changes the record's kind, and if
	// the call fails halfway the first leaves a working name behind.
	keep, ok := -1, false
	for i, r := range existing {
		if strings.EqualFold(r.Type, want.Type) {
			keep, ok = i, true
			break
		}
	}
	if !ok && len(existing) > 0 {
		keep = 0
	}

	switch {
	case keep < 0:
		if err := c.do(ctx, "POST", "/zones/"+c.zoneID+"/dns_records", want.body(), nil); err != nil {
			return Change{}, err
		}
		ch.Action = Created
	case existing[keep].same(want):
		ch.Action = Unchanged
	default:
		path := "/zones/" + c.zoneID + "/dns_records/" + existing[keep].ID
		if err := c.do(ctx, "PUT", path, want.body(), nil); err != nil {
			return Change{}, err
		}
		ch.Action = Updated
	}
	c.logf("cloudflare: %s", ch)

	// Duplicates are removed after the record we keep is correct, so a failure here
	// leaves a name that resolves to the right place plus one that does not, rather
	// than a name with nothing at all.
	for i, r := range existing {
		if i == keep {
			continue
		}
		if err := c.deleteRecord(ctx, r); err != nil {
			return ch, err
		}
	}
	return ch, nil
}

// deleteByName removes every address record at name.
func (c *Cloudflare) deleteByName(ctx context.Context, name string) ([]Change, error) {
	existing, err := c.list(ctx, name)
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, r := range existing {
		if err := c.deleteRecord(ctx, r); err != nil {
			return changes, err
		}
		changes = append(changes, Change{
			Action: Deleted, Name: r.Name, Type: r.Type, Content: r.Content,
		})
	}
	return changes, nil
}

// deleteRecord removes one record. A record that is already gone (81044, or a bare
// 404) is the outcome we wanted, not a failure.
func (c *Cloudflare) deleteRecord(ctx context.Context, r record) error {
	err := c.do(ctx, "DELETE", "/zones/"+c.zoneID+"/dns_records/"+r.ID, nil, nil)
	switch {
	case err == nil:
		c.logf("cloudflare: deleted %s %s -> %s", r.Type, r.Name, r.Content)
		return nil
	case Status(err) == 404, Code(err, 81044):
		return nil
	default:
		return err
	}
}

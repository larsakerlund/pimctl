// Resolving --role, --scope and --key into rows, for a run with no terminal to
// pick in. The rule that matters is that exactness wins: at this tenant's scale
// a bare substring is a footgun, so an exact role-name match beats one, and
// every selection says which happened. The interactive picker is interactive.go;
// the keys these selectors match on are defined in row.go.

package cli

import (
	"fmt"
	"strings"
)

// selectByName resolves --role/--scope filters, preferring exact role-name
// matches over substrings.
//
// A bare substring is a footgun at this tenant's scale: --role Contributor
// matches Contributor, Cost Management Contributor, Resource Policy Contributor
// and Storage Blob Data Contributor — 52 of 136 rows. So an exact
// case-insensitive name match wins outright when one exists, and the caller is
// always told how many of how many matched, and how.
func selectByName(rows []row, roleFilters, scopeFilters []string) (selected []row, report string) {
	scoped := rows
	if len(scopeFilters) > 0 {
		scoped = filterRows(rows, nil, scopeFilters)
	}
	if len(roleFilters) == 0 {
		return scoped, fmt.Sprintf("matched %d of %d eligible roles by scope", len(scoped), len(rows))
	}

	var exact []row
	for _, r := range scoped {
		for _, f := range roleFilters {
			if strings.EqualFold(r.Elig.RoleName(), strings.TrimSpace(f)) {
				exact = append(exact, r)
				break
			}
		}
	}
	if len(exact) > 0 {
		return exact, fmt.Sprintf("matched %d of %d eligible roles (exact role-name match)", len(exact), len(rows))
	}

	sub := filterRows(scoped, roleFilters, nil)
	return sub, fmt.Sprintf("matched %d of %d eligible roles (substring match — no role is named exactly %s)",
		len(sub), len(rows), quoteList(roleFilters))
}

// quoteList renders the filters a report line has to name as `"a" or "b"`.
// They are quoted because most role names contain spaces, and an unquoted list
// of them reads as one long filter.
func quoteList(in []string) string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return strings.Join(out, " or ")
}

// minKeyPrefix is the shortest unambiguous prefix of a selection key a user may
// type; the keys themselves are eight characters.
const minKeyPrefix = 4

// selectByKeys resolves --key selectors. A key may be given in full or as any
// unambiguous prefix of at least four characters.
//
// A row selected twice — `--key abc12345 --key abc1`, the full key and a prefix
// of it — is returned once. Without that the run fires two concurrent PUTs for
// one role, which is the collision DedupeRows exists to prevent everywhere else.
func selectByKeys(rows []row, keys []string) ([]row, error) {
	var out []row
	seen := map[string]bool{}
	for _, raw := range keys {
		k := strings.ToLower(strings.TrimSpace(raw))
		if len(k) < minKeyPrefix {
			return nil, fmt.Errorf(
				"key %q is too short; give at least 4 characters of the key shown by `pimctl list`",
				raw,
			)
		}
		var hits []row
		for _, r := range rows {
			if strings.HasPrefix(r.SelectionKey(), k) {
				hits = append(hits, r)
			}
		}
		switch len(hits) {
		case 0:
			return nil, fmt.Errorf("no eligible role has a key starting with %q (see `pimctl list`)", raw)
		case 1:
			if key := hits[0].SelectionKey(); !seen[key] {
				seen[key] = true
				out = append(out, hits[0])
			}
		default:
			var names []string
			for _, h := range hits {
				names = append(names, h.SelectionKey()+" "+h.Elig.RoleName())
			}
			return nil, fmt.Errorf("key %q is ambiguous; it matches %s", raw, strings.Join(names, ", "))
		}
	}
	return out, nil
}

// filterRows keeps rows whose role name matches any of roleFilters and whose
// scope (id or display name) matches any of scopeFilters. Both lists are
// case-insensitive substring matches; an empty list matches everything.
func filterRows(rows []row, roleFilters, scopeFilters []string) []row {
	var out []row
	for _, r := range rows {
		if !matchAny(r.Elig.RoleName(), roleFilters) {
			continue
		}
		if len(scopeFilters) > 0 &&
			!matchAny(r.Elig.Properties.Scope, scopeFilters) &&
			!matchAny(r.Elig.ScopeName(), scopeFilters) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// matchAny reports whether value contains any of filters, case-insensitively.
// An empty filter list matches everything, so a caller can pass the filters it
// was handed without special-casing "none given".
func matchAny(value string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	v := strings.ToLower(value)
	for _, f := range filters {
		if strings.Contains(v, strings.ToLower(f)) {
			return true
		}
	}
	return false
}

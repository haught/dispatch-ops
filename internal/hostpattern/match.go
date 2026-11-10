package hostpattern

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Inventory is what Match needs of an inventory: its group tree and the
// hosts a job would get (enabled ones). Order doesn't matter; Match orders
// names as Ansible does when it reads the inventory the runner writes.
type Inventory struct {
	Groups []Group
	Hosts  []string
}

// Group is a group's name, its child groups, and its direct member hosts.
type Group struct {
	Name     string
	Children []string
	Hosts    []string
}

// Match returns the hosts in inv that Ansible would select with the limit
// pattern, following ansible.inventory.manager, and the terms of the
// pattern that matched no host (Ansible warns about those). A blank
// pattern selects every host. It returns an error for a term Ansible would
// reject, such as an invalid regular expression or an out-of-range index.
//
// Ansible's regular expressions are Python's; one that Go's regexp can't
// compile (a look-around, say) is reported as an error. The implicit
// localhost that Ansible adds when a pattern names it isn't modelled.
func Match(inv Inventory, pattern string) (hosts, unmatched []string, err error) {
	m := newMatcher(inv)
	terms := splitPattern(pattern)
	if len(terms) == 0 {
		return slices.Clone(m.groupHosts("all")), nil, nil
	}
	for _, t := range orderTerms(terms) {
		if _, ok := m.hostSet[t]; ok {
			// Ansible takes a term that is exactly a host name as that host.
			hosts = union(hosts, []string{t})
			continue
		}
		that, err := m.matchOne(t)
		if err != nil {
			return nil, nil, err
		}
		if len(that) == 0 && t != "all" {
			unmatched = append(unmatched, t)
		}
		switch t[0] {
		case '!':
			hosts = filter(hosts, that, false)
		case '&':
			hosts = filter(hosts, that, true)
		default:
			hosts = union(hosts, that)
		}
	}
	return hosts, unmatched, nil
}

// splitPattern mirrors ansible.inventory.manager.split_host_pattern: a
// pattern with a comma is split on commas; one that is a single address
// (an IPv6 address, or host:port) is one term; otherwise it is split on
// colons and whitespace, keeping [...] subscripts whole.
func splitPattern(pattern string) []string {
	var parts []string
	switch {
	case strings.Contains(pattern, ","):
		parts = strings.Split(pattern, ",")
	case isAddress(strings.TrimSpace(pattern)):
		parts = []string{pattern}
	default:
		parts = colonTermRE.FindAllString(pattern, -1)
	}
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var (
	colonTermRE    = regexp.MustCompile(`(?:[^\s:\[\]]|\[[^\]]*\])+`)
	hostPortRE     = regexp.MustCompile(`^(?:\[[^\]]+\]|[A-Za-z0-9._-]+(?:\[[^\]]*\][A-Za-z0-9._-]*)*):[0-9]+$`)
	subscriptRE    = regexp.MustCompile(`^(.+)\[(?:(-?[0-9]+)|([0-9]+)([:-])([0-9]*))\]$`)
	globCharacters = "*?["
)

// isAddress reports whether s is a single host address that contains
// colons: an IPv6 address (possibly [bracketed]) or a name with a port.
func isAddress(s string) bool {
	if !strings.Contains(s, ":") {
		return false
	}
	if ip := net.ParseIP(strings.Trim(s, "[]")); ip != nil {
		return true
	}
	return hostPortRE.MatchString(s)
}

// orderTerms puts plain terms first, then intersections (&), then
// exclusions (!). With no plain term, the pattern starts from all hosts.
func orderTerms(terms []string) []string {
	var plain, and, not []string
	for _, t := range terms {
		switch t[0] {
		case '!':
			not = append(not, t)
		case '&':
			and = append(and, t)
		default:
			plain = append(plain, t)
		}
	}
	if len(plain) == 0 {
		plain = []string{"all"}
	}
	return slices.Concat(plain, and, not)
}

type matcher struct {
	groups     map[string]Group
	groupOrder []string // all, ungrouped, then the order Ansible adds groups in
	hostOrder  []string // the order Ansible adds hosts in
	hostSet    map[string]struct{}
	ungrouped  []string
	cache      map[string][]string
}

// newMatcher orders groups and hosts the way Ansible's YAML inventory
// plugin adds them when reading the runner's inventory, whose keys
// encoding/json sorts. Every group is defined under all.children; a group
// lists its children by name only, which adds those groups but none of
// their hosts. all's own hosts (every host) are read after its children.
func newMatcher(inv Inventory) *matcher {
	m := &matcher{
		groups:  map[string]Group{},
		hostSet: map[string]struct{}{},
		cache:   map[string][]string{},
	}
	for _, g := range inv.Groups {
		g.Children = sortedCopy(g.Children)
		g.Hosts = sortedCopy(g.Hosts)
		m.groups[g.Name] = g
	}
	names := make([]string, 0, len(m.groups))
	for name := range m.groups {
		names = append(names, name)
	}
	slices.Sort(names)

	seenGroup := map[string]bool{}
	addGroup := func(name string) {
		if _, ok := m.groups[name]; ok && !seenGroup[name] {
			seenGroup[name] = true
			m.groupOrder = append(m.groupOrder, name)
		}
	}
	grouped := map[string]bool{}
	for _, name := range names {
		addGroup(name)
		g := m.groups[name]
		for _, c := range g.Children {
			addGroup(c)
		}
		for _, h := range g.Hosts {
			grouped[h] = true
			m.addHost(h)
		}
	}
	all := sortedCopy(inv.Hosts)
	for _, h := range all {
		m.addHost(h)
		if !grouped[h] {
			m.ungrouped = append(m.ungrouped, h)
		}
	}
	m.groupOrder = append([]string{"all", "ungrouped"}, m.groupOrder...)
	// all's own hosts are every host, in sorted order; they come first in
	// its host list, so its descendants add nothing after them.
	m.groups["all"] = Group{Name: "all", Hosts: all}
	m.groups["ungrouped"] = Group{Name: "ungrouped", Hosts: m.ungrouped}
	return m
}

func (m *matcher) addHost(h string) {
	if _, ok := m.hostSet[h]; !ok {
		m.hostSet[h] = struct{}{}
		m.hostOrder = append(m.hostOrder, h)
	}
}

// groupHosts returns a group's hosts as Ansible's Group.get_hosts does:
// its own, then its descendants' breadth first, each host once.
func (m *matcher) groupHosts(name string) []string {
	var out []string
	seenHost := map[string]bool{}
	seenGroup := map[string]bool{name: true}
	queue := []string{name}
	for len(queue) > 0 {
		g := m.groups[queue[0]]
		queue = queue[1:]
		for _, h := range g.Hosts {
			if !seenHost[h] {
				seenHost[h] = true
				out = append(out, h)
			}
		}
		for _, c := range g.Children {
			if !seenGroup[c] {
				seenGroup[c] = true
				queue = append(queue, c)
			}
		}
	}
	return out
}

// matchOne returns the hosts one term (without its & or !) selects, as
// InventoryManager._match_one_pattern does.
func (m *matcher) matchOne(term string) ([]string, error) {
	if term[0] == '&' || term[0] == '!' {
		term = term[1:]
	}
	if term == "" {
		return nil, nil
	}
	if hosts, ok := m.cache[term]; ok {
		return hosts, nil
	}
	expr, sub, err := splitSubscript(term)
	if err != nil {
		return nil, err
	}
	hosts, err := m.enumerate(expr)
	if err != nil {
		return nil, err
	}
	hosts, err = applySubscript(term, hosts, sub)
	if err != nil {
		return nil, err
	}
	m.cache[term] = hosts
	return hosts, nil
}

// enumerate returns the hosts of the groups expr matches and, if it
// matched no group or is a regex or glob, the hosts it matches by name.
// Like Ansible, it keeps duplicates, which a subscript then counts.
func (m *matcher) enumerate(expr string) ([]string, error) {
	re, err := compileTerm(expr)
	if err != nil {
		return nil, err
	}
	var out []string
	matchedGroup := false
	for _, g := range m.groupOrder {
		if re.MatchString(g) {
			matchedGroup = true
			out = append(out, m.groupHosts(g)...)
		}
	}
	if !matchedGroup || expr[0] == '~' || strings.ContainsAny(expr, ".?*[") {
		for _, h := range m.hostOrder {
			if re.MatchString(h) {
				out = append(out, h)
			}
		}
	}
	return out, nil
}

// compileTerm compiles a term as Ansible does: ~ starts a regular
// expression matched from the start of a name; anything else is a shell
// glob (fnmatch) that must match the whole name.
func compileTerm(expr string) (*regexp.Regexp, error) {
	src := `^(?s:` + globToRegexp(expr) + `)\z`
	if expr[0] == '~' {
		src = `^(?:` + expr[1:] + `)`
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return nil, fmt.Errorf("invalid host pattern %q: %v", expr, err)
	}
	return re, nil
}

// globToRegexp translates a shell glob as Python's fnmatch.translate does.
func globToRegexp(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := i + 1
			if j < len(glob) && glob[j] == '!' {
				j++
			}
			if j < len(glob) && glob[j] == ']' {
				j++
			}
			for j < len(glob) && glob[j] != ']' {
				j++
			}
			if j >= len(glob) {
				b.WriteString(`\[`)
				continue
			}
			class := glob[i+1 : j]
			i = j
			neg := strings.HasPrefix(class, "!")
			if neg {
				class = class[1:]
			}
			class = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, `^`, `\^`).Replace(class)
			if neg {
				b.WriteString("[^" + class + "]")
			} else {
				b.WriteString("[" + class + "]")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

type subscript struct {
	start int
	end   *int // nil for a single index
}

// splitSubscript splits a trailing [i], [i:j], [i:] or [i-j] off a term,
// as InventoryManager._split_subscript does. Regexes have none.
func splitSubscript(term string) (string, *subscript, error) {
	if term[0] == '~' {
		return term, nil, nil
	}
	m := subscriptRE.FindStringSubmatch(term)
	if m == nil {
		return term, nil, nil
	}
	if m[2] != "" {
		i, err := strconv.Atoi(m[2])
		if err != nil {
			return "", nil, fmt.Errorf("invalid subscript in %q", term)
		}
		return m[1], &subscript{start: i}, nil
	}
	start, err := strconv.Atoi(m[3])
	if err != nil {
		return "", nil, fmt.Errorf("invalid subscript in %q", term)
	}
	end := -1
	if m[5] != "" {
		if end, err = strconv.Atoi(m[5]); err != nil {
			return "", nil, fmt.Errorf("invalid subscript in %q", term)
		}
	}
	return m[1], &subscript{start: start, end: &end}, nil
}

// applySubscript selects from hosts as InventoryManager._apply_subscript
// does: a range's end is inclusive, an open end means the last host, and
// (as in Ansible) an end of 0 makes the range a single index.
func applySubscript(term string, hosts []string, sub *subscript) ([]string, error) {
	if sub == nil {
		return hosts, nil
	}
	n := len(hosts)
	if sub.end != nil && *sub.end != 0 {
		start, end := sub.start, *sub.end
		if end == -1 {
			end = n - 1
		}
		// Python slice hosts[start:end+1], clamped.
		end = min(end+1, n)
		if start >= end {
			return nil, nil
		}
		return hosts[start:end], nil
	}
	i := sub.start
	if i < 0 {
		i += n
	}
	if i < 0 || i >= n {
		return nil, fmt.Errorf("no hosts matched the subscripted pattern %q (it has %d hosts)", term, n)
	}
	return []string{hosts[i]}, nil
}

func union(hosts, more []string) []string {
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		seen[h] = true
	}
	for _, h := range more {
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// filter keeps the hosts that are (keep) or aren't (!keep) in that.
func filter(hosts, that []string, keep bool) []string {
	in := make(map[string]bool, len(that))
	for _, h := range that {
		in[h] = true
	}
	out := hosts[:0:0]
	for _, h := range hosts {
		if in[h] == keep {
			out = append(out, h)
		}
	}
	return out
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

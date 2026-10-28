package front

import (
	"regexp"
	"slices"
	"sort"
)

// hostRe is a DNS name relative to a private zone: dot-separated lower-case
// labels of 1..63 characters.
var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// Registry is the port registry of one shared NLB, as a consumer keeps it in
// configuration: one listener per service, and the port is the service's
// identity (an NLB cannot route by name). It is plain data, so a consumer can
// check the file before any program builds a [Listener] from it.
type Registry struct {
	// Listeners maps a service name ("buildkit-arm64") to its listener port.
	Listeners map[string]int
	// Names maps a DNS name, relative to the private zone, to the listener
	// it serves. Optional.
	Names map[string]string
}

// Validate holds the registry to what [SharedNLB] and [Attach] need: listener
// names the NLB accepts (1..32 letters, digits and hyphens, no hyphen at
// either end), ports in 1..65535, one service per port, at most
// [MaxListeners] listeners, and DNS names that are well-formed and serve a
// listener of the registry. It reports every finding at once.
func (r Registry) Validate() error {
	p := &problems{}

	if len(r.Listeners) > MaxListeners {
		p.add("%d listeners: an NLB takes at most %d", len(r.Listeners), MaxListeners)
	}

	byPort := map[int][]string{}

	for _, name := range sortedKeys(r.Listeners) {
		port := r.Listeners[name]

		if !nameRe.MatchString(name) {
			p.add("listener %q: want 1..32 letters, digits and hyphens, no hyphen at either end", name)
		}

		if !validPort(port) {
			p.add("listener %q: port %d: want 1..65535", name, port)

			continue
		}

		byPort[port] = append(byPort[port], name)
	}

	for _, host := range sortedKeys(r.Names) {
		if !hostRe.MatchString(host) {
			p.add("name %q: want dot-separated lower-case labels, relative to the private zone", host)
		}

		if _, ok := r.Listeners[r.Names[host]]; !ok {
			p.add("name %q serves listener %q, which is not in listeners", host, r.Names[host])
		}
	}

	ports := make([]int, 0, len(byPort))
	for port := range byPort {
		ports = append(ports, port)
	}

	slices.Sort(ports)

	for _, port := range ports {
		if names := byPort[port]; len(names) > 1 {
			p.add("port %d is held by %v: the port is the service's identity, so a port belongs to one service", port, names)
		}
	}

	return p.err("front registry")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

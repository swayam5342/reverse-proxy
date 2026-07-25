// Package discovery resolves upstream names to targets. Only static,
// config-defined upstreams are supported for now (per AGENTS.md non-goals,
// no service mesh); the Resolver interface exists so a Consul/DNS-backed
// implementation can be swapped in later without touching callers.
package discovery

import (
	"fmt"
	"net/url"
)

// Resolver resolves a logical upstream name to its target URL.
type Resolver interface {
	Resolve(name string) (*url.URL, error)
}

// StaticResolver resolves names from a fixed, config-loaded map. It never
// changes at runtime, matching the "config loaded once at startup" rule.
type StaticResolver struct {
	targets map[string]*url.URL
}

// NewStaticResolver builds a StaticResolver from name->URL pairs.
func NewStaticResolver(upstreams map[string]string) (*StaticResolver, error) {
	targets := make(map[string]*url.URL, len(upstreams))
	for name, raw := range upstreams {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("discovery: parsing upstream %q url %q: %w", name, raw, err)
		}
		targets[name] = u
	}
	return &StaticResolver{targets: targets}, nil
}

// Resolve implements Resolver.
func (s *StaticResolver) Resolve(name string) (*url.URL, error) {
	u, ok := s.targets[name]
	if !ok {
		return nil, fmt.Errorf("discovery: unknown upstream %q", name)
	}
	return u, nil
}

package adapters

import (
	"fmt"
	"sort"
)

// constructor builds an adapter from a SiteConfig.
type constructor func(SiteConfig) Adapter

// registry maps an adapter type to its constructor. It is intentionally a
// plain var so tests (or downstream forks) can register extra architectures.
var registry = map[string]constructor{
	TypeNexusPHPAPI:     newNexusPHPAPI,
	TypeNexusPHPClassic: newNexusPHPClassic,
	TypeMTeam:           newMTeam,
}

// New builds the adapter for the given SiteConfig. An empty Type defaults to
// TypeNexusPHPAPI; an unknown Type is an error.
func New(cfg SiteConfig) (Adapter, error) {
	cfg.Normalize()
	typ := cfg.Type
	if typ == "" {
		typ = TypeNexusPHPAPI
	}
	ctor, ok := registry[typ]
	if !ok {
		return nil, newAdapterError(nil, 0,
			fmt.Sprintf("unknown target type %q (available: %s)", typ, joinTypes(Types())), "")
	}
	return ctor(cfg), nil
}

// Register installs (or replaces) a constructor for an adapter type.
func Register(typ string, ctor constructor) {
	registry[typ] = ctor
}

// Types returns the registered adapter types, sorted.
func Types() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinTypes(types []string) string {
	out := ""
	for i, t := range types {
		if i > 0 {
			out += ", "
		}
		out += t
	}
	return out
}

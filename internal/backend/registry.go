package backend

import (
	"fmt"
	"sort"
)

// Factory constructs a backend from a connection's name and options.
type Factory func(name string, opts map[string]string) (Backend, error)

var registry = map[string]Factory{}

// Register makes a backend kind available to the rest of the program.
func Register(kind string, f Factory) {
	registry[kind] = f
}

// Kinds lists the registered backend kinds.
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build instantiates a backend of the given kind.
func Build(kind, name string, opts map[string]string) (Backend, error) {
	f, ok := registry[kind]
	if !ok {
		// Allow simple aliases.
		switch kind {
		case "ftps":
			kind = "ftp"
			f, ok = registry[kind]
		case "sftp":
			f, ok = registry["sftp"]
		}
		if !ok {
			return nil, fmt.Errorf("unknown connection type %q", kind)
		}
	}
	if opts == nil {
		opts = map[string]string{}
	}
	return f(name, opts)
}

// Opt is a tiny helper for reading options with a fallback.
func Opt(opts map[string]string, key, def string) string {
	if v, ok := opts[key]; ok && v != "" {
		return v
	}
	return def
}

// OptBool reads a boolean option.
func OptBool(opts map[string]string, key string, def bool) bool {
	v, ok := opts[key]
	if !ok || v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

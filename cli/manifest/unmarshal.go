package manifest

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// UnmarshalYAML accepts either a plain address string
// (`"/api": localhost:8000`) or a run-service mapping
// (`"/shop": {command: npm run dev, port: 3000, ...}`).
func (r *Route) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var target string
		if err := value.Decode(&target); err != nil {
			return err
		}
		r.Target = target
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("route value must be an address string or a command mapping")
	}

	var raw struct {
		Command     string            `yaml:"command"`
		Cwd         string            `yaml:"cwd"`
		Port        int               `yaml:"port"`
		Environment map[string]string `yaml:"environment"`
	}
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("invalid route mapping: %w", err)
	}
	r.Command = raw.Command
	r.Cwd = raw.Cwd
	r.Port = raw.Port
	r.Environment = raw.Environment

	// A route entry is a single-key mapping {path: value}; the path itself
	// is filled in by the Environment decode below.
	return nil
}

// unmarshalRoutes decodes the routes mapping where each key is a path and
// its value an address string or run-service mapping.
func unmarshalRoutes(node *yaml.Node) ([]Route, error) {
	if node.Kind != yaml.MappingNode || len(node.Content)%2 != 0 {
		return nil, fmt.Errorf("routes must be a mapping of /<path>: <address|command>")
	}
	routes := make([]Route, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		var path string
		if err := node.Content[i].Decode(&path); err != nil {
			return nil, err
		}
		var r Route
		if err := r.UnmarshalYAML(node.Content[i+1]); err != nil {
			return nil, fmt.Errorf("route %s: %w", path, err)
		}
		r.Path = path
		routes = append(routes, r)
	}
	return routes, nil
}

func (e *Environment) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("environment must be a mapping with host and routes")
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		keyNode, valNode := value.Content[i], value.Content[i+1]
		switch keyNode.Value {
		case "host":
			if err := valNode.Decode(&e.Host); err != nil {
				return err
			}
		case "protect":
			if err := valNode.Decode(&e.Protect); err != nil {
				return err
			}
		case "routes":
			routes, err := unmarshalRoutes(valNode)
			if err != nil {
				return err
			}
			e.Routes = routes
		default:
			return fmt.Errorf("unknown environment field %q", keyNode.Value)
		}
	}
	return nil
}

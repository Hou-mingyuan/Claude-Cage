// stage-proxy imports a selected route without copying a subscription URL or
// another application's listener, routing, and account settings.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"

	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func main() {
	input := flag.String("input", "", "Clash/Mihomo YAML containing nodes")
	selected := flag.String("select", "", "Exact node display name; empty lists matching nodes")
	match := flag.String("match", "美国|美國|United States|(?i:us|america)|🇺🇸", "List filter")
	profile := flag.String("output-profile", "", "Private staging directory, not an active installation")
	flag.Parse()
	if err := run(*input, *selected, *match, *profile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(input, selected, match, profile string) error {
	data, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	nodes, err := managedproxy.Parse(data, "existing-airport-route")
	if err != nil {
		return err
	}
	if selected == "" {
		filter, err := regexp.Compile(match)
		if err != nil {
			return err
		}
		var names []map[string]string
		for _, n := range nodes {
			if filter.MatchString(n.Name) {
				names = append(names, map[string]string{"name": n.Name, "type": n.Type})
			}
		}
		return json.NewEncoder(os.Stdout).Encode(names)
	}
	if profile == "" {
		return fmt.Errorf("output-profile is required for staging")
	}
	if _, err := os.Stat(managedproxy.Dir(profile)); !os.IsNotExist(err) {
		return fmt.Errorf("staging proxy directory already exists; preserve it and choose another path")
	}
	s := managedproxy.Store{Version: 1, Port: 17896, Auth: managedproxy.Token(), Revision: managedproxy.Token()}
	s.Sources = []managedproxy.Source{{ID: "existing-airport-route", Name: "Existing airport selected route", Nodes: nodes}}
	for _, n := range nodes {
		if n.Name == selected {
			s.Selected = n.ID
		}
	}
	route, err := s.Route()
	if err != nil {
		return err
	}
	var needed []managedproxy.Node
	for _, id := range route {
		node, _ := s.Find(id)
		needed = append(needed, node)
	}
	s.Chain, s.Sources[0].Nodes = route, needed
	if err := s.Save(profile); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"staged": true, "selected": selected, "nodes": len(needed)})
}

package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Storage is one entry of the node's storage list.
type Storage struct {
	Name    string `json:"storage"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Active  int    `json:"active"`
	Enabled *int   `json:"enabled"`
	Avail   int64  `json:"avail"`
}

// StoragesCmd lists the node's storages that can hold container root disks ("Container" content).
func StoragesCmd(node string) Cmd {
	return Cmd{"pvesh", []string{"get", "/nodes/" + node + "/storage", "--content", "rootdir", "--output-format", "json"}}
}

// linkedCloneTypes can snapshot a template, which runners' fast linked clones need.
var linkedCloneTypes = []string{"lvmthin", "zfspool", "rbd"}

// LinkedClones reports whether runners can be linked clones on a storage of this type; on others
// they need full_clone.
func LinkedClones(storageType string) bool { return slices.Contains(linkedCloneTypes, storageType) }

// RootdirStorages lists active storages that can hold containers, best first: linked-clone capable
// ones, then the most free space.
func (h *Host) RootdirStorages(ctx context.Context) ([]Storage, error) {
	out, err := h.run(ctx, StoragesCmd(h.Node))
	if err != nil {
		return nil, err
	}
	var all []Storage
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("parse storage list: %w", err)
	}
	var ok []Storage
	for _, s := range all {
		if s.Active == 1 && (s.Enabled == nil || *s.Enabled == 1) && slices.Contains(strings.Split(s.Content, ","), "rootdir") {
			ok = append(ok, s)
		}
	}
	sort.SliceStable(ok, func(i, j int) bool {
		if a, b := LinkedClones(ok[i].Type), LinkedClones(ok[j].Type); a != b {
			return a
		}
		return ok[i].Avail > ok[j].Avail
	})
	return ok, nil
}

// RootfsStorage returns the storage container disks go on: the configured one after checking it can
// hold containers, or the best available one when none is configured.
func (h *Host) RootfsStorage(ctx context.Context, configured string) (string, error) {
	list, err := h.RootdirStorages(ctx)
	if err != nil {
		return "", fmt.Errorf("list storages: %w", err)
	}
	names := make([]string, len(list))
	for i, s := range list {
		names[i] = s.Name
	}
	if configured == "" {
		if len(list) == 0 {
			return "", fmt.Errorf("no active storage on node %s can hold containers; enable \"Container\" content on one under Datacenter > Storage", h.Node)
		}
		return list[0].Name, nil
	}
	if slices.Contains(names, configured) {
		return configured, nil
	}
	hint := "none of this node's active storages has \"Container\" content; enable it under Datacenter > Storage"
	if len(names) > 0 {
		hint = "ones that can: " + strings.Join(names, ", ") + " (or leave it empty to pick one automatically)"
	}
	return "", fmt.Errorf("proxmox.storage %q can't hold containers (not active, or no \"Container\" content); %s", configured, hint)
}

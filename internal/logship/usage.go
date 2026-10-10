package logship

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

// UsageService is the service label of usage samples. They are kept apart from {service="pve-runners"}
// so reading the logs doesn't mean scrolling past a sample per runner every round.
const UsageService = "pve-runners-usage"

// Job is what a usage sample records about the job a runner is running.
type Job struct {
	Repo, Name, Workflow string
	RunID                int64
}

// sample is one usage line. Sizes are bytes; the I/O rates are bytes per second since the previous
// sample of the same guest.
type sample struct {
	CPUCores     float64 `json:"cpu_cores"`
	CPULimit     float64 `json:"cpu_limit"`
	MemBytes     int64   `json:"mem_bytes"`
	MemLimit     int64   `json:"mem_limit_bytes"`
	DiskBytes    int64   `json:"disk_bytes"`
	DiskLimit    int64   `json:"disk_limit_bytes"`
	NetInBps     float64 `json:"net_in_bps"`
	NetOutBps    float64 `json:"net_out_bps"`
	DiskReadBps  float64 `json:"disk_read_bps"`
	DiskWriteBps float64 `json:"disk_write_bps"`
	Job          string  `json:"job,omitempty"`
	Workflow     string  `json:"workflow,omitempty"`
	RunID        int64   `json:"run_id,omitempty"`
}

// counters is what a guest's previous sample needs to turn the next one into rates.
type counters struct {
	at                                time.Time
	netIn, netOut, diskRead, diskWrit int64
}

// usage builds one stream per running runner from the guest list Proxmox already reported.
func (s *Shipper) usage(runners []pve.Guest) []stream {
	now := s.Now()
	next := map[int]counters{}
	var out []stream
	for _, g := range runners {
		if g.Status != "running" {
			continue
		}
		c := counters{now, g.NetIn, g.NetOut, g.DiskRead, g.DiskWrite}
		next[g.VMID] = c
		smp := sample{CPUCores: round(g.CPU * g.MaxCPU), CPULimit: g.MaxCPU, MemBytes: g.Mem, MemLimit: g.MaxMem,
			DiskBytes: g.Disk, DiskLimit: g.MaxDisk}
		if p, ok := s.prev[g.VMID]; ok {
			if secs := now.Sub(p.at).Seconds(); secs > 0 {
				smp.NetInBps, smp.NetOutBps = rate(p.netIn, c.netIn, secs), rate(p.netOut, c.netOut, secs)
				smp.DiskReadBps, smp.DiskWriteBps = rate(p.diskRead, c.diskRead, secs), rate(p.diskWrit, c.diskWrit, secs)
			}
		}
		labels := map[string]string{"service": UsageService, "host": s.HostID, "vmid": strconv.Itoa(g.VMID),
			"runner": g.Name, "pool": g.TagValue("pool")}
		if s.Jobs != nil {
			if j, ok := s.Jobs(g.Name); ok {
				// The repo is a label so dashboards can filter on it cheaply; job names and run IDs are
				// many and short-lived, so they stay in the line.
				labels["repo"] = j.Repo
				smp.Job, smp.Workflow, smp.RunID = j.Name, j.Workflow, j.RunID
			}
		}
		line, _ := json.Marshal(smp)
		out = append(out, stream{labels, [][2]string{{strconv.FormatInt(now.UnixNano(), 10), string(line)}}})
	}
	s.prev = next
	return out
}

// rate is bytes per second between two readings of a counter; a counter that went down was reset.
func rate(from, to int64, secs float64) float64 {
	if to < from {
		return 0
	}
	return round(float64(to-from) / secs)
}

func round(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

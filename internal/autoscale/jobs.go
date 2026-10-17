package autoscale

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset"
)

// JobInfo is the job a runner is running, as GitHub's job messages describe it.
type JobInfo struct {
	Repo     string // owner/name
	Job      string // the job's display name, such as "unit (2, 4)"
	Workflow string // the workflow file and ref
	RunID    int64
}

// Jobs remembers which job each runner has, so usage samples can be labelled with it. It only knows
// jobs started since this process began; a runner mid-job across a restart goes unlabelled.
type Jobs struct {
	mu sync.Mutex
	m  map[string]JobInfo
}

func (j *Jobs) record(job scaleset.Job) {
	if j == nil || job.RunnerName == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.m == nil {
		j.m = map[string]JobInfo{}
	}
	switch job.Type {
	case "JobStarted":
		j.m[job.RunnerName] = JobInfo{Repo: job.OwnerName + "/" + job.RepositoryName, Job: job.JobDisplayName,
			Workflow: job.JobWorkflowRef, RunID: job.WorkflowRunID}
	case "JobCompleted":
		delete(j.m, job.RunnerName)
	}
}

// Of returns the job the runner is running.
func (j *Jobs) Of(runner string) (JobInfo, bool) {
	if j == nil {
		return JobInfo{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	info, ok := j.m[runner]
	return info, ok
}

// Job tags name what a runner is building, so the Proxmox guest list shows it: repo-<name> and one of
// branch-<name>, pr-<number> or tag-<name>. The owner is already in owner-<org>.
var jobTagPrefixes = []string{"repo-", "branch-", "pr-", "tag-"}

// JobTags returns the job tags for a started job. The ref comes from the job's workflow ref, so it is
// left out when the workflow lives in another repository (a reusable workflow names its own ref).
func JobTags(j scaleset.Job) []string {
	tags := []string{"repo-" + tagSafe(j.RepositoryName)}
	repo, ref, ok := strings.Cut(j.JobWorkflowRef, "@")
	if !ok || !strings.HasPrefix(strings.ToLower(repo), strings.ToLower(j.OwnerName+"/"+j.RepositoryName+"/")) {
		return tags
	}
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		tags = append(tags, "branch-"+tagSafe(b))
	} else if t, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		tags = append(tags, "tag-"+tagSafe(t))
	} else if pr, ok := strings.CutPrefix(ref, "refs/pull/"); ok {
		n, _, _ := strings.Cut(pr, "/")
		tags = append(tags, "pr-"+tagSafe(n))
	}
	return tags
}

// jobNotes is the guest description while it runs a job: the exact names tags can't hold.
func jobNotes(j scaleset.Job) string {
	repo := j.OwnerName + "/" + j.RepositoryName
	s := "GitHub Actions runner managed by pve-runners\n\nRunning " + repo
	if j.JobDisplayName != "" {
		s += ", job \"" + j.JobDisplayName + "\""
	}
	if j.JobWorkflowRef != "" {
		s += "\nWorkflow: " + j.JobWorkflowRef
	}
	if j.WorkflowRunID != 0 {
		s += "\nRun: https://github.com/" + repo + "/actions/runs/" + strconv.FormatInt(j.WorkflowRunID, 10)
	}
	return s
}

// label tags the runner's guest with the job it started. It only adds information, so a failure is
// logged and nothing else changes.
func (l *Listener) label(ctx context.Context, j scaleset.Job) {
	if j.RunnerName == "" || l.Host == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	gs, err := l.Host.Runners(ctx)
	if err != nil {
		l.log().Warn("labelling runner failed", "listener", l.Name(), "runner", j.RunnerName, "err", err)
		return
	}
	i := slices.IndexFunc(gs, func(g pve.Guest) bool { return g.Name == j.RunnerName })
	if i < 0 {
		return
	}
	g := gs[i]
	// A persistent runner keeps its guest between jobs, so the previous job's tags go first.
	tags := slices.DeleteFunc(g.Tags(), func(t string) bool {
		return slices.ContainsFunc(jobTagPrefixes, func(p string) bool { return strings.HasPrefix(t, p) })
	})
	tags = append(tags, JobTags(j)...)
	if err := l.Host.Exec(ctx, pve.LXCLabelCmd(g.VMID, tags, jobNotes(j))); err != nil {
		l.log().Warn("labelling runner failed", "listener", l.Name(), "vmid", g.VMID, "err", err)
	}
}

// tagSafe keeps what Proxmox allows in a tag ([a-z0-9_+.-]); '/' becomes '-' so branch paths stay readable.
func tagSafe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '/':
			b.WriteRune('-')
		}
	}
	return b.String()
}

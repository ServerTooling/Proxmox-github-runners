package autoscale

import (
	"sync"

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

// Package scaleset is a hand-written client for GitHub's runner scale set API (the protocol
// actions-runner-controller uses). A scale set is a named runner pool that workflows target with
// runs-on: <name>; its listener long-polls GitHub for queued jobs over outbound HTTPS only, which is
// what lets a host with no inbound access autoscale.
//
// Flow: a runner registration token is traded at api.github.com/actions/runner-registration for the
// Actions service URL and an admin token; the scale set is found or created; a message session is
// opened; then messages (jobs and statistics) are polled, acknowledged, and jobs acquired.
package scaleset

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiVersion       = "6.0-preview"
	scaleSetEndpoint = "_apis/runtime/runnerscalesets"
	// The default runner group is always ID 1 in the Actions service.
	DefaultGroupID = 1
)

// ServiceError is a non-2xx answer from GitHub or the Actions service.
type ServiceError struct {
	Status   int
	Method   string
	Path     string
	TypeName string // e.g. AgentExistsException
	Message  string
}

func (e *ServiceError) Error() string {
	s := fmt.Sprintf("actions service %s %s: %d", e.Method, e.Path, e.Status)
	if e.TypeName != "" {
		s += " " + e.TypeName
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// ErrSessionConflict means another listener (another host, or a previous run that hasn't expired)
// already holds this scale set's message session.
var ErrSessionConflict = errors.New("another listener already has a session for this scale set")

func statusOf(err error) int {
	var se *ServiceError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// Client talks to the Actions service for one org (ConfigURL https://github.com/<org>) or one
// repository (https://github.com/<owner>/<repo>).
type Client struct {
	APIBase   string // https://api.github.com
	ConfigURL string
	// RegToken returns a fresh runner registration token for ConfigURL's org or repo.
	RegToken func(ctx context.Context) (string, error)
	HTTP     *http.Client
	Now      func() time.Time

	mu       sync.Mutex
	service  string
	admin    string
	adminExp time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// jwtExpiry reads exp from a JWT without verifying it (the token is only ever sent back to GitHub).
func jwtExpiry(tok string) (time.Time, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// connect returns the service URL and admin token, re-registering a minute before expiry.
func (c *Client) connect(ctx context.Context) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.admin != "" && c.now().Add(time.Minute).Before(c.adminExp) {
		return c.service, c.admin, nil
	}
	reg, err := c.RegToken(ctx)
	if err != nil {
		return "", "", fmt.Errorf("runner registration token: %w", err)
	}
	var out struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	body := map[string]string{"url": c.ConfigURL, "runner_event": "register"}
	endpoint := strings.TrimRight(c.APIBase, "/") + "/actions/runner-registration"
	// A brand-new registration token can take a moment to propagate, so auth failures are retried.
	for attempt := 0; ; attempt++ {
		err = c.send(ctx, http.MethodPost, endpoint, "RemoteAuth "+reg, nil, body, &out)
		if s := statusOf(err); (s == 401 || s == 403) && attempt < 3 {
			if werr := sleep(ctx, time.Duration(attempt+1)*time.Second); werr != nil {
				return "", "", werr
			}
			continue
		}
		break
	}
	if err != nil {
		return "", "", err
	}
	if out.URL == "" || out.Token == "" {
		return "", "", errors.New("runner registration returned no Actions service URL or token")
	}
	exp, ok := jwtExpiry(out.Token)
	if !ok {
		exp = c.now().Add(50 * time.Minute)
	}
	c.service, c.admin, c.adminExp = strings.TrimRight(out.URL, "/"), out.Token, exp
	return c.service, c.admin, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// send performs one request. header may add or override headers; body and out are JSON.
func (c *Client) send(ctx context.Context, method, rawURL, auth string, header http.Header, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pve-runners")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // the service sometimes sends a BOM
	if resp.StatusCode >= 300 {
		se := &ServiceError{Status: resp.StatusCode, Method: method, Path: req.URL.Path}
		var m struct {
			TypeName string `json:"typeName"`
			Message  string `json:"message"`
		}
		if json.Unmarshal(data, &m) == nil {
			se.TypeName, se.Message = m.TypeName, m.Message
		} else {
			se.Message = strings.TrimSpace(string(data))
		}
		return se
	}
	if out != nil && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// call calls an Actions service path with the admin token.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	base, admin, err := c.connect(ctx)
	if err != nil {
		return err
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("api-version", apiVersion)
	return c.send(ctx, method, base+"/"+path+"?"+query.Encode(), "Bearer "+admin, nil, body, out)
}

type Label struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type RunnerSetting struct {
	Ephemeral     bool `json:"ephemeral"`
	DisableUpdate bool `json:"disableUpdate"`
}

type ScaleSet struct {
	ID            int64         `json:"id,omitempty"`
	Name          string        `json:"name"`
	RunnerGroupID int64         `json:"runnerGroupId"`
	Labels        []Label       `json:"labels"`
	RunnerSetting RunnerSetting `json:"RunnerSetting"` // capitalized on the wire
}

// RunnerGroupID resolves a runner group name in the Actions service ("" or Default is always 1).
func (c *Client) RunnerGroupID(ctx context.Context, name string) (int64, error) {
	if name == "" || strings.EqualFold(name, "default") {
		return DefaultGroupID, nil
	}
	var out struct {
		Count int `json:"count"`
		Value []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := c.call(ctx, http.MethodGet, "_apis/runtime/runnergroups/", url.Values{"groupName": {name}}, nil, &out); err != nil {
		return 0, err
	}
	if out.Count == 0 || len(out.Value) == 0 {
		return 0, fmt.Errorf("runner group %q not found", name)
	}
	return out.Value[0].ID, nil
}

// EnsureScaleSet finds the scale set by name in the group, creating it if needed. Runners are always
// ephemeral (one job each) and never self-update; the host installs the runner version it pins.
func (c *Client) EnsureScaleSet(ctx context.Context, name string, groupID int64, labels []string) (*ScaleSet, error) {
	var list struct {
		Count int        `json:"count"`
		Value []ScaleSet `json:"value"`
	}
	q := url.Values{"runnerGroupId": {strconv.FormatInt(groupID, 10)}, "name": {name}}
	if err := c.call(ctx, http.MethodGet, scaleSetEndpoint, q, nil, &list); err != nil {
		return nil, err
	}
	switch {
	case list.Count > 1:
		return nil, fmt.Errorf("found %d scale sets named %q", list.Count, name)
	case list.Count == 1 && len(list.Value) == 1:
		return &list.Value[0], nil
	}
	ss := ScaleSet{Name: name, RunnerGroupID: groupID, RunnerSetting: RunnerSetting{Ephemeral: true, DisableUpdate: true}}
	for _, l := range labels {
		ss.Labels = append(ss.Labels, Label{Type: "System", Name: l})
	}
	var out ScaleSet
	if err := c.call(ctx, http.MethodPost, scaleSetEndpoint, nil, ss, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// JITConfig is a single-use runner registration inside a scale set.
type JITConfig struct {
	Runner struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encodedJITConfig"`
}

// GenerateJIT registers a runner and returns its single-use config. A runner that never took a job
// (it crashed, or its container was destroyed first) stays registered, and its name comes back when
// the container ID is reused. Runner names hold the host ID and a container ID that is free right
// now, so a registration with this name is stale: remove it and register again.
func (c *Client) GenerateJIT(ctx context.Context, scaleSetID int64, name, workFolder string) (*JITConfig, error) {
	var out JITConfig
	path := scaleSetEndpoint + "/" + strconv.FormatInt(scaleSetID, 10) + "/generatejitconfig"
	body := map[string]string{"name": name, "workFolder": workFolder}
	err := c.call(ctx, http.MethodPost, path, nil, body, &out)
	if !isRunnerExists(err) {
		if err != nil {
			return nil, err
		}
		return &out, nil
	}
	if rerr := c.removeRunnerNamed(ctx, name); rerr != nil {
		// Never let a stale name block spawning: register under a unique name instead. The stale
		// registration is offline and harmless, and GitHub drops it on its own.
		body["name"] = name + "-" + strconv.FormatInt(c.now().Unix()%1e6, 36)
		if err2 := c.call(ctx, http.MethodPost, path, nil, body, &out); err2 != nil {
			return nil, errors.Join(err, fmt.Errorf("remove stale runner %s: %w", name, rerr), err2)
		}
		return &out, nil
	}
	if err := c.call(ctx, http.MethodPost, path, nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// agentsEndpoint lists and removes registered runners; the runtime API has no runner lookup.
const agentsEndpoint = "_apis/distributedtask/pools/0/agents"

func isRunnerExists(err error) bool {
	var se *ServiceError
	return errors.As(err, &se) && se.Status == http.StatusConflict && strings.Contains(se.TypeName, "AgentExistsException")
}

func (c *Client) removeRunnerNamed(ctx context.Context, name string) error {
	var list struct {
		Count int `json:"count"`
		Value []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := c.call(ctx, http.MethodGet, agentsEndpoint, url.Values{"agentName": {name}}, nil, &list); err != nil {
		return err
	}
	for _, r := range list.Value {
		if r.Name != name {
			continue
		}
		if err := c.call(ctx, http.MethodDelete, agentsEndpoint+"/"+strconv.FormatInt(r.ID, 10),
			nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// ErrRunnerBusy means GitHub refused to remove a runner because it has a job.
var ErrRunnerBusy = errors.New("runner has a job")

// RemoveRunner unregisters a runner so no job can be given to it any more. GitHub refuses while the
// runner has a job (ErrRunnerBusy), which is what makes removing an idle runner safe.
func (c *Client) RemoveRunner(ctx context.Context, id int64) error {
	err := c.call(ctx, http.MethodDelete, agentsEndpoint+"/"+strconv.FormatInt(id, 10), nil, nil, nil)
	var se *ServiceError
	if errors.As(err, &se) && strings.Contains(se.TypeName, "JobStillRunning") {
		return fmt.Errorf("%w: %v", ErrRunnerBusy, err)
	}
	return err
}

type Statistics struct {
	TotalAvailableJobs     int `json:"totalAvailableJobs"`
	TotalAcquiredJobs      int `json:"totalAcquiredJobs"`
	TotalAssignedJobs      int `json:"totalAssignedJobs"`
	TotalRunningJobs       int `json:"totalRunningJobs"`
	TotalRegisteredRunners int `json:"totalRegisteredRunners"`
	TotalBusyRunners       int `json:"totalBusyRunners"`
	TotalIdleRunners       int `json:"totalIdleRunners"`
}

// Session is an open message session: the right to receive this scale set's jobs.
type Session struct {
	c          *Client
	scaleSetID int64
	mu         sync.Mutex
	info       sessionInfo
}

type sessionInfo struct {
	SessionID   string      `json:"sessionId"`
	OwnerName   string      `json:"ownerName"`
	QueueURL    string      `json:"messageQueueUrl"`
	QueueToken  string      `json:"messageQueueAccessToken"`
	Statistics  *Statistics `json:"statistics"`
	RunnerScale *ScaleSet   `json:"runnerScaleSet,omitempty"`
}

func (c *Client) sessionPath(id int64) string {
	return scaleSetEndpoint + "/" + strconv.FormatInt(id, 10) + "/sessions"
}

// CreateSession opens the message session. owner names this host in GitHub's records.
func (c *Client) CreateSession(ctx context.Context, scaleSetID int64, owner string) (*Session, error) {
	s := &Session{c: c, scaleSetID: scaleSetID}
	err := c.call(ctx, http.MethodPost, c.sessionPath(scaleSetID), nil, map[string]string{"ownerName": owner}, &s.info)
	if statusOf(err) == http.StatusConflict {
		return nil, fmt.Errorf("%w: %v", ErrSessionConflict, err)
	}
	if err != nil {
		return nil, err
	}
	if s.info.QueueURL == "" || s.info.QueueToken == "" {
		return nil, errors.New("message session has no queue URL or token")
	}
	return s, nil
}

// Statistics are the counts GitHub sent when the session opened.
func (s *Session) Statistics() Statistics {
	if s.info.Statistics == nil {
		return Statistics{}
	}
	return *s.info.Statistics
}

func (s *Session) refresh(ctx context.Context) error {
	var info sessionInfo
	path := s.c.sessionPath(s.scaleSetID) + "/" + url.PathEscape(s.info.SessionID)
	if err := s.c.call(ctx, http.MethodPatch, path, nil, nil, &info); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if info.QueueURL != "" {
		s.info.QueueURL = info.QueueURL
	}
	if info.QueueToken != "" {
		s.info.QueueToken = info.QueueToken
	}
	return nil
}

// queue runs a request authorized by the message queue token, refreshing the session once on 401.
func (s *Session) queue(ctx context.Context, do func(token, queueURL string) error) error {
	s.mu.Lock()
	tok, q := s.info.QueueToken, s.info.QueueURL
	s.mu.Unlock()
	err := do(tok, q)
	if statusOf(err) != http.StatusUnauthorized {
		return err
	}
	if rerr := s.refresh(ctx); rerr != nil {
		return errors.Join(err, rerr)
	}
	s.mu.Lock()
	tok, q = s.info.QueueToken, s.info.QueueURL
	s.mu.Unlock()
	return do(tok, q)
}

// Message is one batch from the queue.
type Message struct {
	ID         int64       `json:"messageId"`
	Type       string      `json:"messageType"`
	Body       string      `json:"body"`
	Statistics *Statistics `json:"statistics"`
}

// Job is one entry of a RunnerScaleSetJobMessages body.
type Job struct {
	Type            string `json:"messageType"` // JobAvailable | JobAssigned | JobStarted | JobCompleted
	RunnerRequestID int64  `json:"runnerRequestId"`
	RepositoryName  string `json:"repositoryName"`
	OwnerName       string `json:"ownerName"`
	JobWorkflowRef  string `json:"jobWorkflowRef"`
	JobDisplayName  string `json:"jobDisplayName"`
	WorkflowRunID   int64  `json:"workflowRunId"`
	EventName       string `json:"eventName"`
	RunnerName      string `json:"runnerName,omitempty"`
	Result          string `json:"result,omitempty"`
}

// Jobs decodes a message body (a JSON string holding an array of jobs).
func (m *Message) Jobs() ([]Job, error) {
	if m.Type != "RunnerScaleSetJobMessages" {
		return nil, fmt.Errorf("unsupported message type %q", m.Type)
	}
	if strings.TrimSpace(m.Body) == "" {
		return nil, nil
	}
	var jobs []Job
	if err := json.Unmarshal([]byte(m.Body), &jobs); err != nil {
		return nil, fmt.Errorf("parse message %d: %w", m.ID, err)
	}
	return jobs, nil
}

// GetMessage long-polls for the next message after lastID; nil, nil means the poll timed out empty.
func (s *Session) GetMessage(ctx context.Context, lastID int64, maxCapacity int) (*Message, error) {
	var msg *Message
	err := s.queue(ctx, func(token, queueURL string) error {
		u, err := url.Parse(queueURL)
		if err != nil {
			return err
		}
		if lastID > 0 {
			q := u.Query()
			q.Set("lastMessageId", strconv.FormatInt(lastID, 10))
			u.RawQuery = q.Encode()
		}
		h := http.Header{}
		h.Set("Accept", "application/json; api-version="+apiVersion)
		h.Set("X-ScaleSetMaxCapacity", strconv.Itoa(maxCapacity))
		var m Message
		if err := s.c.send(ctx, http.MethodGet, u.String(), "Bearer "+token, h, nil, &m); err != nil {
			return err
		}
		if m.ID != 0 || m.Type != "" {
			msg = &m
		} else {
			msg = nil
		}
		return nil
	})
	return msg, err
}

// DeleteMessage acknowledges a message so it isn't delivered again.
func (s *Session) DeleteMessage(ctx context.Context, id int64) error {
	return s.queue(ctx, func(token, queueURL string) error {
		u, err := url.Parse(queueURL)
		if err != nil {
			return err
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/" + strconv.FormatInt(id, 10)
		return s.c.send(ctx, http.MethodDelete, u.String(), "Bearer "+token, nil, nil, nil)
	})
}

// AcquirableJobs lists the jobs waiting for a runner of this scale set. Each job's JobAvailable message
// goes to one session only, so a job that became available before the current session (the listener
// restarted, or stopped reading its queue) is found only here.
func (c *Client) AcquirableJobs(ctx context.Context, scaleSetID int64) ([]Job, error) {
	var out struct {
		Value []Job `json:"value"`
	}
	err := c.call(ctx, http.MethodGet, scaleSetEndpoint+"/"+strconv.FormatInt(scaleSetID, 10)+"/acquirablejobs", nil, nil, &out)
	return out.Value, err
}

// AcquireJobs claims jobs for this scale set; it returns the request IDs GitHub granted.
func (s *Session) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	base, _, err := s.c.connect(ctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Value []int64 `json:"value"`
	}
	path := scaleSetEndpoint + "/" + strconv.FormatInt(s.scaleSetID, 10) + "/acquirejobs?api-version=" + apiVersion
	err = s.queue(ctx, func(token, _ string) error {
		return s.c.send(ctx, http.MethodPost, base+"/"+path, "Bearer "+token, nil, requestIDs, &out)
	})
	return out.Value, err
}

// Close deletes the session so another listener (or this one after a restart) can take over at once.
func (s *Session) Close(ctx context.Context) error {
	path := s.c.sessionPath(s.scaleSetID) + "/" + url.PathEscape(s.info.SessionID)
	return s.c.call(ctx, http.MethodDelete, path, nil, nil, nil)
}

// GitHubURL is the web URL that runner config URLs are built on, from the REST API base:
// api.github.com serves github.com, and GitHub Enterprise Server serves its API under /api/v3.
func GitHubURL(apiBase string) string {
	base := strings.TrimRight(apiBase, "/")
	if base == "https://api.github.com" {
		return "https://github.com"
	}
	return strings.TrimSuffix(base, "/api/v3")
}

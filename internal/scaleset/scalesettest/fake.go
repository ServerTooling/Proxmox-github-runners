// Package scalesettest is an in-process fake of GitHub's runner registration endpoint and the Actions
// service scale set API. It checks auth and api-version the way the service does, queues messages
// for the listener, and records every call for assertions.
package scalesettest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	AdminToken = "admin"
	QueueToken = "queue"
)

type Call struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Header http.Header
	Body   string
}

// Job is one entry in a queued message body, in the service's wire format.
type Job struct {
	Type            string `json:"messageType"`
	RunnerRequestID int64  `json:"runnerRequestId"`
	RepositoryName  string `json:"repositoryName"`
	OwnerName       string `json:"ownerName"`
}

type ScaleSet struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	RunnerGroupID int64             `json:"runnerGroupId"`
	Labels        []json.RawMessage `json:"labels"`
	RunnerSetting json.RawMessage   `json:"RunnerSetting"`
}

type Fake struct {
	*httptest.Server
	// RegToken is the registration token runner-registration accepts.
	RegToken string
	// Groups maps runner group names to IDs for the runnergroups lookup.
	Groups map[string]int64
	// PollWait is how long an empty message poll blocks before answering 202.
	PollWait time.Duration

	mu        sync.Mutex
	calls     []Call
	scaleSets []*ScaleSet
	sessions  map[string]int64 // session ID -> scale set ID
	conflicts int              // CreateSession answers 409 this many more times
	expire    bool             // the next queue call answers 401 until the session is refreshed
	queueTok  string
	nextMsg   int64
	queue     []map[string]any
	acquired  []int64
	jit       []string
	runners   map[string]int64 // registered runner name -> ID
	removed   []int64
	wake      chan struct{}
	// NoRunnerLookup makes the runner lookup answer 404, as if the endpoint were unavailable.
	NoRunnerLookup bool
	// Assigned is reported as totalAssignedJobs in session and message statistics.
	Assigned int
}

func New() *Fake {
	f := &Fake{RegToken: "regtok", Groups: map[string]int64{}, PollWait: 20 * time.Millisecond,
		sessions: map[string]int64{}, runners: map[string]int64{}, queueTok: QueueToken, wake: make(chan struct{}, 1)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// AdminJWT is an unsigned JWT with the given expiry; the client only reads exp.
func AdminJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + "." + enc([]byte("sig"))
}

func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CallsTo returns the calls whose method and path match (path may end in * as a prefix match).
func (f *Fake) CallsTo(method, path string) []Call {
	var out []Call
	for _, c := range f.Calls() {
		if c.Method != method {
			continue
		}
		if p, ok := strings.CutSuffix(path, "*"); ok && strings.HasPrefix(c.Path, p) || c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func (f *Fake) ScaleSets() []ScaleSet {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ScaleSet, len(f.scaleSets))
	for i, s := range f.scaleSets {
		out[i] = *s
	}
	return out
}

// AddScaleSet pre-creates a scale set, as if another run had made it.
func (f *Fake) AddScaleSet(name string, groupID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := int64(len(f.scaleSets) + 1)
	f.scaleSets = append(f.scaleSets, &ScaleSet{ID: id, Name: name, RunnerGroupID: groupID})
	return id
}

func (f *Fake) Sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func (f *Fake) Acquired() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.acquired...)
}

// JITNames lists the runner names JIT configs were generated for.
func (f *Fake) JITNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.jit...)
}

// AddRunner registers a runner by name, as if an earlier container had taken it and never ran a job.
func (f *Fake) AddRunner(name string, id int64) {
	f.mu.Lock()
	f.runners[name] = id
	f.mu.Unlock()
}

// Removed lists the IDs of runners deleted through the agents endpoint.
func (f *Fake) Removed() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.removed...)
}

// Conflict makes the next n session creations fail with 409.
func (f *Fake) Conflict(n int) {
	f.mu.Lock()
	f.conflicts = n
	f.mu.Unlock()
}

// ExpireQueueToken makes the queue token stale; the client must refresh the session.
func (f *Fake) ExpireQueueToken() {
	f.mu.Lock()
	f.expire = true
	f.mu.Unlock()
}

// Push queues one message holding jobs, and sets the assigned-job count its statistics report.
func (f *Fake) Push(assigned int, jobs ...Job) {
	f.mu.Lock()
	f.Assigned = assigned
	body, _ := json.Marshal(jobs)
	f.nextMsg++
	f.queue = append(f.queue, map[string]any{"messageId": f.nextMsg, "messageType": "RunnerScaleSetJobMessages",
		"body": string(body), "statistics": f.stats()})
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Pending is the number of queued messages not yet deleted.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}

func (f *Fake) stats() map[string]int {
	return map[string]int{"totalAssignedJobs": f.Assigned}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The real service prefixes some bodies with a UTF-8 BOM; so does the fake.
	io.WriteString(w, "\xef\xbb\xbf")
	json.NewEncoder(w).Encode(v)
}

func svcError(w http.ResponseWriter, status int, typeName, msg string) {
	writeJSON(w, status, map[string]string{"typeName": typeName, "message": msg})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: auth,
		Header: r.Header.Clone(), Body: string(body)})
	f.mu.Unlock()

	if r.URL.Path == "/actions/runner-registration" && r.Method == http.MethodPost {
		if auth != "RemoteAuth "+f.RegToken {
			svcError(w, 401, "", "bad registration token")
			return
		}
		writeJSON(w, 200, map[string]string{"url": f.URL + "/svc/", "token": AdminJWT(time.Now().Add(time.Hour))})
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/queue/"); ok {
		f.serveQueue(w, r, rest)
		return
	}
	if r.URL.Query().Get("api-version") != "6.0-preview" {
		svcError(w, 400, "", "missing api-version")
		return
	}
	if r.URL.Path == "/svc/_apis/distributedtask/pools/0/agents" && r.Method == http.MethodGet {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.NoRunnerLookup {
			svcError(w, 404, "", "")
			return
		}
		name := r.URL.Query().Get("agentName")
		id, ok := f.runners[name]
		if !ok {
			writeJSON(w, 200, map[string]any{"count": 0, "value": []any{}})
			return
		}
		writeJSON(w, 200, map[string]any{"count": 1, "value": []any{map[string]any{"id": id, "name": name}}})
		return
	}
	if idStr, ok := strings.CutPrefix(r.URL.Path, "/svc/_apis/distributedtask/pools/0/agents/"); ok && r.Method == http.MethodDelete {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		f.mu.Lock()
		defer f.mu.Unlock()
		for name, rid := range f.runners {
			if rid == id {
				delete(f.runners, name)
				f.removed = append(f.removed, id)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		svcError(w, 404, "TaskAgentNotFoundException", "no such runner")
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/svc/_apis/runtime/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	// acquirejobs is the one service call made with the queue token.
	if strings.HasSuffix(rest, "/acquirejobs") {
		f.acquire(w, auth, body)
		return
	}
	if !strings.HasPrefix(auth, "Bearer ") || !strings.Contains(auth, ".") {
		svcError(w, 401, "", "admin token required")
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case parts[0] == "runnergroups" && r.Method == http.MethodGet:
		name := r.URL.Query().Get("groupName")
		id, ok := f.Groups[name]
		if !ok {
			writeJSON(w, 200, map[string]any{"count": 0, "value": []any{}})
			return
		}
		writeJSON(w, 200, map[string]any{"count": 1, "value": []any{map[string]any{"id": id, "name": name}}})
	case parts[0] == "runnerscalesets" && len(parts) == 1 && r.Method == http.MethodGet:
		gid, _ := strconv.ParseInt(r.URL.Query().Get("runnerGroupId"), 10, 64)
		var found []*ScaleSet
		for _, s := range f.scaleSets {
			if s.Name == r.URL.Query().Get("name") && s.RunnerGroupID == gid {
				found = append(found, s)
			}
		}
		writeJSON(w, 200, map[string]any{"count": len(found), "value": found})
	case parts[0] == "runnerscalesets" && len(parts) == 1 && r.Method == http.MethodPost:
		var s ScaleSet
		if err := json.Unmarshal(body, &s); err != nil {
			svcError(w, 400, "", err.Error())
			return
		}
		s.ID = int64(len(f.scaleSets) + 1)
		f.scaleSets = append(f.scaleSets, &s)
		writeJSON(w, 200, s)
	case parts[0] == "runnerscalesets" && len(parts) == 3 && parts[2] == "generatejitconfig":
		var req struct{ Name string }
		json.Unmarshal(body, &req)
		if _, ok := f.runners[req.Name]; ok {
			svcError(w, 409, "Microsoft.TeamFoundation.DistributedTask.WebApi.TaskAgentExistsException",
				"A runner with the name "+req.Name+" already exists.")
			return
		}
		f.jit = append(f.jit, req.Name)
		id := int64(100 + len(f.jit))
		f.runners[req.Name] = id
		writeJSON(w, 200, map[string]any{"runner": map[string]any{"id": id, "name": req.Name},
			"encodedJITConfig": "jit-" + req.Name})
	case parts[0] == "runnerscalesets" && len(parts) == 3 && parts[2] == "sessions" && r.Method == http.MethodPost:
		if f.conflicts > 0 {
			f.conflicts--
			svcError(w, 409, "TaskAgentSessionConflictException", "session exists")
			return
		}
		ssid, _ := strconv.ParseInt(parts[1], 10, 64)
		sid := fmt.Sprintf("sess-%d", len(f.calls))
		f.sessions[sid] = ssid
		writeJSON(w, 200, map[string]any{"sessionId": sid, "messageQueueUrl": f.URL + "/queue/" + sid,
			"messageQueueAccessToken": f.queueTok, "statistics": f.stats()})
	case parts[0] == "runnerscalesets" && len(parts) == 4 && parts[2] == "sessions" && r.Method == http.MethodPatch:
		f.expire = false
		f.queueTok = QueueToken + "-refreshed"
		writeJSON(w, 200, map[string]any{"sessionId": parts[3], "messageQueueUrl": f.URL + "/queue/" + parts[3],
			"messageQueueAccessToken": f.queueTok})
	case parts[0] == "runnerscalesets" && len(parts) == 4 && parts[2] == "sessions" && r.Method == http.MethodDelete:
		delete(f.sessions, parts[3])
		w.WriteHeader(http.StatusNoContent)
	default:
		svcError(w, 404, "", "no such route "+r.Method+" "+r.URL.Path)
	}
}

func (f *Fake) queueAuthOK(auth string) bool {
	return !f.expire && auth == "Bearer "+f.queueTok
}

func (f *Fake) serveQueue(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	f.mu.Lock()
	if !f.queueAuthOK(r.Header.Get("Authorization")) {
		f.mu.Unlock()
		svcError(w, 401, "", "queue token expired")
		return
	}
	if _, ok := f.sessions[parts[0]]; !ok {
		f.mu.Unlock()
		svcError(w, 404, "TaskAgentSessionExpiredException", "no session")
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 2 {
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		for i, m := range f.queue {
			if m["messageId"] == id {
				f.queue = append(f.queue[:i], f.queue[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	last, _ := strconv.ParseInt(r.URL.Query().Get("lastMessageId"), 10, 64)
	next := func() map[string]any {
		for _, m := range f.queue {
			if m["messageId"].(int64) > last {
				return m
			}
		}
		return nil
	}
	m := next()
	f.mu.Unlock()
	if m == nil {
		select {
		case <-f.wake:
		case <-time.After(f.PollWait):
		case <-r.Context().Done():
		}
		f.mu.Lock()
		m = next()
		f.mu.Unlock()
	}
	if m == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, 200, m)
}

func (f *Fake) acquire(w http.ResponseWriter, auth string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.queueAuthOK(auth) {
		svcError(w, 401, "", "queue token required")
		return
	}
	var ids []int64
	if err := json.Unmarshal(body, &ids); err != nil {
		svcError(w, 400, "", err.Error())
		return
	}
	f.acquired = append(f.acquired, ids...)
	writeJSON(w, 200, map[string]any{"count": len(ids), "value": ids})
}

package actions

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers the calls actions/scaleset makes, from the GitHub App
// token exchange to the message queue, so the adapter is tested against the
// library itself rather than against a fake of it.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// scaleSets are the scale sets that exist, by name.
	scaleSets map[string]map[string]any
	// requests are "METHOD path" of every call to the Actions service.
	requests []string
	// jitBody answers generatejitconfig.
	jitBody string
	// removeStatus and removeBody answer a runner's DELETE, by ID.
	removeStatus map[string]int
	removeBody   map[string]string
	// runnersByName answers a lookup by name.
	runnersByName map[string]int
	// messages are served in turn; nil is an empty poll.
	messages []*string
	acquired []int64
	// acquirable answers acquirablejobs; empty is a 204.
	acquirable string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{
		t:             t,
		scaleSets:     map[string]map[string]any{},
		removeStatus:  map[string]int{},
		removeBody:    map[string]string{},
		runnersByName: map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimSuffix(r.URL.Path, "/")
	body, _ := io.ReadAll(r.Body)
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if s, ok := v.(string); ok {
			io.WriteString(w, s)
			return
		}
		json.NewEncoder(w).Encode(v)
	}
	later := time.Now().Add(time.Hour)

	// The token exchange, on the GitHub API of a GHES-style URL.
	switch {
	case r.Method == "POST" && path == "/api/v3/app/installations/7/access_tokens":
		reply(201, map[string]any{"token": "ghs_installation", "expires_at": later})
		return
	case r.Method == "POST" && path == "/api/v3/orgs/example-org/actions/runners/registration-token":
		reply(201, map[string]any{"token": "registration", "expires_at": later})
		return
	case r.Method == "POST" && path == "/api/v3/actions/runner-registration":
		reply(200, map[string]any{"url": f.srv.URL + "/tenant", "token": adminToken(later)})
		return
	}

	f.requests = append(f.requests, r.Method+" "+path)
	const sets = "/tenant/_apis/runtime/runnerscalesets"
	const agents = "/tenant/_apis/distributedtask/pools/0/agents"
	switch {
	case r.Method == "GET" && path == "/tenant/_apis/runtime/runnergroups":
		reply(200, map[string]any{"count": 1, "value": []any{map[string]any{"id": 7, "name": r.URL.Query().Get("groupName")}}})
	case r.Method == "GET" && path == sets:
		s, ok := f.scaleSets[r.URL.Query().Get("name")]
		if !ok {
			reply(200, map[string]any{"count": 0, "value": []any{}})
			return
		}
		reply(200, map[string]any{"count": 1, "value": []any{s}})
	case r.Method == "POST" && path == sets, r.Method == "PATCH" && path == sets+"/42":
		var s map[string]any
		json.Unmarshal(body, &s)
		s["id"] = 42
		f.scaleSets[s["name"].(string)] = s
		reply(200, s)
	case r.Method == "POST" && path == sets+"/42/generatejitconfig":
		reply(200, f.jitBody)
	case r.Method == "DELETE" && strings.HasPrefix(path, agents+"/"):
		id := strings.TrimPrefix(path, agents+"/")
		status := f.removeStatus[id]
		if status == 0 {
			status = 204
		}
		if status == 204 {
			w.WriteHeader(204)
			return
		}
		reply(status, f.removeBody[id])
	case r.Method == "GET" && path == agents:
		id, ok := f.runnersByName[r.URL.Query().Get("agentName")]
		if !ok {
			reply(200, map[string]any{"count": 0, "value": []any{}})
			return
		}
		reply(200, map[string]any{"count": 1, "value": []any{map[string]any{"id": id, "name": r.URL.Query().Get("agentName")}}})
	case r.Method == "POST" && path == sets+"/42/sessions":
		reply(200, map[string]any{
			"sessionId":               "6f1c1a5e-0000-4000-8000-000000000001",
			"ownerName":               "host",
			"messageQueueUrl":         f.srv.URL + "/queue",
			"messageQueueAccessToken": "queue-token",
			"statistics":              map[string]any{"totalAssignedJobs": 3, "totalRunningJobs": 1},
		})
	case r.Method == "DELETE" && strings.HasPrefix(path, sets+"/42/sessions/"):
		w.WriteHeader(204)
	case r.Method == "GET" && path == sets+"/42/acquirablejobs":
		if f.acquirable == "" {
			w.WriteHeader(204)
			return
		}
		reply(200, f.acquirable)
	case r.Method == "POST" && path == sets+"/42/acquirejobs":
		var ids []int64
		json.Unmarshal(body, &ids)
		f.acquired = append(f.acquired, ids...)
		reply(200, map[string]any{"count": len(ids), "value": ids})
	case r.Method == "GET" && path == "/queue":
		if len(f.messages) == 0 || f.messages[0] == nil {
			if len(f.messages) > 0 {
				f.messages = f.messages[1:]
			}
			w.WriteHeader(202)
			return
		}
		m := *f.messages[0]
		f.messages = f.messages[1:]
		reply(200, m)
	case r.Method == "DELETE" && strings.HasPrefix(path, "/queue/"):
		w.WriteHeader(204)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(500)
	}
}

// adminToken is a JWT carrying only an expiry, which is all the library
// reads from it.
func adminToken(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + "." + enc([]byte("signature"))
}

var appKey = sync.OnceValue(func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
})

func (f *fakeGitHub) client(t *testing.T) Client {
	t.Helper()
	return f.clientLogging(t, nil)
}

func (f *fakeGitHub) clientLogging(t *testing.T, log *slog.Logger) Client {
	t.Helper()
	c, err := New(f.srv.URL+"/example-org", AppCredentials{ID: "123", InstallationID: 7, PrivateKey: appKey()}, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeGitHub) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.requests
	f.requests = nil
	return out
}

func labelNames(s map[string]any) []string {
	var out []string
	for _, l := range s["labels"].([]any) {
		out = append(out, l.(map[string]any)["name"].(string))
	}
	return out
}

func TestEnsureScaleSetCreatesUpdatesAndReuses(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(t)
	ctx := context.Background()

	id, change, err := c.EnsureScaleSet(ctx, ScaleSetSpec{Name: "linux", RunnerGroup: "default"})
	if err != nil || id != 42 || change != Created {
		t.Fatalf("create: %d, %q, %v", id, change, err)
	}
	created := f.scaleSets["linux"]
	if got := labelNames(created); !slices.Equal(got, []string{"linux"}) {
		t.Errorf("labels %v, want the name", got)
	}
	if created["runnerGroupId"].(float64) != 1 {
		t.Errorf("runner group %v, want the default's 1", created["runnerGroupId"])
	}
	if created["RunnerSetting"].(map[string]any)["disableUpdate"] != true {
		t.Errorf("self-update isn't off: %v", created["RunnerSetting"])
	}
	f.calls()

	// The same labels, in another order: nothing to change.
	f.scaleSets["linux"]["labels"] = []any{map[string]any{"name": "b"}, map[string]any{"name": "a"}}
	if _, change, err := c.EnsureScaleSet(ctx, ScaleSetSpec{Name: "linux", RunnerGroup: "default", Labels: []string{"a", "b"}}); err != nil || change != Unchanged {
		t.Fatalf("matching labels: %q, %v", change, err)
	}
	if calls := f.calls(); slices.ContainsFunc(calls, func(s string) bool { return strings.HasPrefix(s, "PATCH") }) {
		t.Errorf("updated a scale set that matched: %v", calls)
	}

	// Different labels: updated.
	if _, change, err := c.EnsureScaleSet(ctx, ScaleSetSpec{Name: "linux", RunnerGroup: "default", Labels: []string{"a", "c"}}); err != nil || change != Updated {
		t.Fatalf("changed labels: %q, %v", change, err)
	}
	if got := labelNames(f.scaleSets["linux"]); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("labels %v after the update", got)
	}
}

func TestEnsureScaleSetLooksUpItsRunnerGroup(t *testing.T) {
	f := newFakeGitHub(t)
	if _, _, err := f.client(t).EnsureScaleSet(context.Background(), ScaleSetSpec{Name: "linux", RunnerGroup: "builders"}); err != nil {
		t.Fatal(err)
	}
	if got := f.scaleSets["linux"]["runnerGroupId"].(float64); got != 7 {
		t.Errorf("runner group %v, want the looked-up 7", got)
	}
}

func TestGenerateJITConfig(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(t)
	ctx := context.Background()

	f.jitBody = `{"runner":{"id":5,"name":"host-p-0-ab"},"encodedJITConfig":"c2VjcmV0"}`
	jit, err := c.GenerateJITConfig(ctx, 42, "host-p-0-ab")
	if err != nil || jit.RunnerID != 5 || jit.Encoded != "c2VjcmV0" {
		t.Fatalf("got %+v, %v", jit, err)
	}

	f.jitBody = `{"encodedJITConfig":"c2VjcmV0"}`
	if _, err := c.GenerateJITConfig(ctx, 42, "host-p-0-ab"); err == nil {
		t.Error("a config without its runner was accepted")
	}

	// A success whose body can't be decoded. The library quotes whatever
	// its decoder hadn't read, which for a JIT config of a few kilobytes
	// broken near its start is the credentials.
	f.jitBody = `{"runner":{"id":5,!},"encodedJITConfig":"` + strings.Repeat("TOP-SECRET-CREDENTIALS", 200) + `"}`
	_, err = c.GenerateJITConfig(ctx, 42, "host-p-0-ab")
	if err == nil {
		t.Fatal("an undecodable config was accepted")
	}
	if strings.Contains(err.Error(), "TOP-SECRET") {
		t.Errorf("the error quotes the runner's credentials: %v", err)
	}
}

func TestRemoveRunner(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(t)
	ctx := context.Background()

	if err := c.RemoveRunner(ctx, 5, "r5"); err != nil {
		t.Errorf("removing: %v", err)
	}

	f.removeStatus["6"] = 404
	f.removeBody["6"] = `{"typeName":"GitHub.DistributedTask.WebApi.AgentNotFoundException","message":"no such agent"}`
	if err := c.RemoveRunner(ctx, 6, "r6"); err != nil {
		t.Errorf("a registration already gone: %v", err)
	}

	f.removeStatus["7"] = 400
	f.removeBody["7"] = `{"typeName":"GitHub.DistributedTask.WebApi.JobStillRunningException","message":"busy"}`
	if err := c.RemoveRunner(ctx, 7, "r7"); !errors.Is(err, ErrJobStillRunning) {
		t.Errorf("a runner mid-job: %v, want ErrJobStillRunning", err)
	}

	f.removeStatus["8"] = 403
	f.removeBody["8"] = `{"typeName":"Forbidden","message":"no"}`
	if err := c.RemoveRunner(ctx, 8, "r8"); err == nil || errors.Is(err, ErrJobStillRunning) {
		t.Errorf("any other refusal: %v, want an error", err)
	}

	// Without an ID, it is found by name, or there's nothing to remove.
	f.runnersByName["named"] = 9
	f.calls()
	if err := c.RemoveRunner(ctx, 0, "named"); err != nil {
		t.Errorf("by name: %v", err)
	}
	if err := c.RemoveRunner(ctx, 0, "never-registered"); err != nil {
		t.Errorf("by a name never registered: %v", err)
	}
	var deletes []string
	for _, call := range f.calls() {
		if strings.HasPrefix(call, "DELETE") {
			deletes = append(deletes, call)
		}
	}
	if want := []string{"DELETE /tenant/_apis/distributedtask/pools/0/agents/9"}; !slices.Equal(deletes, want) {
		t.Errorf("deleted %v, want %v", deletes, want)
	}
}

func TestSession(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(t)
	ctx := context.Background()

	queued := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	events := []map[string]any{
		{"messageType": "JobAvailable", "runnerRequestId": 1, "queueTime": queued, "acquireJobUrl": "x"},
		{"messageType": "JobAssigned", "runnerRequestId": 2, "queueTime": queued},
		{"messageType": "JobStarted", "runnerRequestId": 3, "runnerName": "r3", "runnerId": 3},
		{"messageType": "JobCompleted", "runnerRequestId": 4, "runnerName": "r4", "result": "canceled"},
	}
	eventsJSON, _ := json.Marshal(events)
	msg, _ := json.Marshal(map[string]any{
		"messageId":   11,
		"messageType": "RunnerScaleSetJobMessages",
		"body":        string(eventsJSON),
		"statistics":  map[string]any{"totalAssignedJobs": 4, "totalRunningJobs": 2},
	})
	m := string(msg)
	f.messages = []*string{nil, &m}

	sess, err := c.OpenSession(ctx, 42, "host")
	if err != nil {
		t.Fatal(err)
	}
	if got := sess.Statistics(); got != (Statistics{AssignedJobs: 3, RunningJobs: 1}) {
		t.Errorf("session statistics %+v", got)
	}

	got, err := sess.GetMessage(ctx, 0, 5)
	if err != nil || got != nil {
		t.Fatalf("an empty poll: %+v, %v", got, err)
	}
	got, err = sess.GetMessage(ctx, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := &Message{
		ID:         11,
		Statistics: Statistics{AssignedJobs: 4, RunningJobs: 2},
		Available:  []Job{{RequestID: 1, QueueTime: queued}},
		Assigned:   []Job{{RequestID: 2, QueueTime: queued}},
		Started:    []Job{{RequestID: 3, RunnerName: "r3"}},
		Completed:  []Job{{RequestID: 4, RunnerName: "r4", Result: "canceled"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("message\n got %+v\nwant %+v", got, want)
	}

	if err := sess.DeleteMessage(ctx, 11); err != nil {
		t.Error(err)
	}
	if err := sess.AcquireJobs(ctx, []int64{1}); err != nil || !slices.Equal(f.acquired, []int64{1}) {
		t.Errorf("acquired %v, %v", f.acquired, err)
	}
	if err := sess.Close(ctx); err != nil {
		t.Error(err)
	}
	calls := f.calls()
	for _, want := range []string{"DELETE /queue/11", "DELETE /tenant/_apis/runtime/runnerscalesets/42/sessions/6f1c1a5e-0000-4000-8000-000000000001"} {
		if !slices.Contains(calls, want) {
			t.Errorf("no %q in %v", want, calls)
		}
	}
}

func TestWithholdJITConfig(t *testing.T) {
	plain := errors.New("request POST /generatejitconfig failed (status=\"403 Forbidden\"): no")
	if got := withholdJITConfig(plain); got != plain {
		t.Errorf("an ordinary error was changed: %v", got)
	}
	for _, msg := range []string{
		"failed to decode runner JIT config: x secret",
		`body {"encodedJITConfig":"secret"}`,
		// As actions/scaleset words one: neither of the above, but on a 200.
		`request POST /generatejitconfig failed(status="200 OK"): invalid character 'T': failed to unmarshal error response body: "secret"`,
	} {
		if got := withholdJITConfig(errors.New(msg)); strings.Contains(got.Error(), "secret") || strings.Contains(got.Error(), msg) {
			t.Errorf("%q came through as %v", msg, got)
		}
	}
}

func TestAcquirableJobs(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(t)
	ctx := context.Background()

	jobs, err := c.AcquirableJobs(ctx, 42)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("none (204): %v, %v", jobs, err)
	}

	queued := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f.acquirable = fmt.Sprintf(`{"count":2,"value":[
		{"messageType":"JobAvailable","runnerRequestId":7,"queueTime":%q,"acquireJobUrl":"x"},
		{"messageType":"JobAvailable","runnerRequestId":8,"queueTime":%q}]}`,
		queued.Format(time.RFC3339), queued.Add(time.Minute).Format(time.RFC3339))
	jobs, err = c.AcquirableJobs(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	want := []Job{{RequestID: 7, QueueTime: queued}, {RequestID: 8, QueueTime: queued.Add(time.Minute)}}
	if fmt.Sprint(jobs) != fmt.Sprint(want) {
		t.Errorf("got %+v, want %+v", jobs, want)
	}
}

func TestDebugLogsRawMessagesButNotTheSessionToken(t *testing.T) {
	f := newFakeGitHub(t)
	var out bytes.Buffer
	c := f.clientLogging(t, slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx := context.Background()

	raw, _ := json.Marshal(map[string]any{
		"messageId": 3, "messageType": "RunnerScaleSetJobMessages", "statistics": map[string]any{},
		"body": `[{"messageType":"JobStarted","runnerName":"r1"}]`,
	})
	msg := string(raw)
	f.messages = []*string{&msg}
	sess, err := c.OpenSession(ctx, 42, "host")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sess.GetMessage(ctx, 0, 1)
	if err != nil || got == nil || len(got.Started) != 1 || got.Started[0].RunnerName != "r1" {
		t.Fatalf("the message didn't survive being logged: %+v, %v", got, err)
	}
	if !strings.Contains(out.String(), `msg="raw message" scale_set_id=42`) || !strings.Contains(out.String(), "JobStarted") {
		t.Errorf("no raw message logged:\n%s", out.String())
	}
	if strings.Contains(out.String(), "queue-token") {
		t.Errorf("the session's access token was logged:\n%s", out.String())
	}

	// Not at info level.
	out.Reset()
	c = f.clientLogging(t, slog.New(slog.NewTextHandler(&out, nil)))
	f.messages = []*string{&msg}
	sess, _ = c.OpenSession(ctx, 42, "host")
	sess.GetMessage(ctx, 0, 1)
	if strings.Contains(out.String(), "raw message") {
		t.Errorf("raw message logged at info level:\n%s", out.String())
	}
}

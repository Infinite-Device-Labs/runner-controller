package actions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"

	"github.com/Infinite-Device-Labs/runner-controller/internal/scaleset"
)

// AppCredentials are a GitHub App's, for one installation.
type AppCredentials struct {
	// ID is the App's ID or its client ID.
	ID             string
	InstallationID int64
	// PrivateKey is the PEM key itself, not its path.
	PrivateKey string
}

// New returns a Client for the scale sets registered at url, an organisation
// or a repository such as https://github.com/example-org. At debug level, log
// also gets each message's raw body.
func New(url string, app AppCredentials, version string, log *slog.Logger) (Client, error) {
	c, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: url,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       app.ID,
			InstallationID: app.InstallationID,
			PrivateKey:     app.PrivateKey,
		},
		SystemInfo: systemInfo(version, 0),
	})
	if err != nil {
		return nil, fmt.Errorf("creating the scale set client for %s: %w", url, err)
	}
	return &client{c: c, version: version, log: log}, nil
}

func systemInfo(version string, scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "runner-controller",
		Version:    version,
		Subsystem:  "controller",
		ScaleSetID: scaleSetID,
	}
}

type client struct {
	c       *scaleset.Client
	version string
	log     *slog.Logger
}

func (c *client) EnsureScaleSet(ctx context.Context, spec ScaleSetSpec) (int, Change, error) {
	groupID := 1 // the default group's ID is always 1
	if spec.RunnerGroup != scaleset.DefaultRunnerGroup {
		g, err := c.c.GetRunnerGroupByName(ctx, spec.RunnerGroup)
		if err != nil {
			return 0, "", fmt.Errorf("finding runner group %q: %w", spec.RunnerGroup, err)
		}
		groupID = g.ID
	}

	want := &scaleset.RunnerScaleSet{
		Name:          spec.Name,
		RunnerGroupID: groupID,
		Labels:        labels(spec),
		// The unit's image pins the runner version; self-update would eat
		// into the job.
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}

	got, err := c.c.GetRunnerScaleSet(ctx, groupID, spec.Name)
	if err != nil {
		return 0, "", fmt.Errorf("looking up scale set %q: %w", spec.Name, err)
	}
	change := Unchanged
	switch {
	case got == nil:
		got, err = c.c.CreateRunnerScaleSet(ctx, want)
		if err != nil {
			return 0, "", fmt.Errorf("creating scale set %q: %w", spec.Name, err)
		}
		change = Created
	case !sameLabels(got.Labels, want.Labels) || got.RunnerSetting != want.RunnerSetting:
		got, err = c.c.UpdateRunnerScaleSet(ctx, got.ID, want)
		if err != nil {
			return 0, "", fmt.Errorf("updating scale set %q: %w", spec.Name, err)
		}
		change = Updated
	}
	c.c.SetSystemInfo(systemInfo(c.version, got.ID))
	return got.ID, change, nil
}

func labels(spec ScaleSetSpec) []scaleset.Label {
	names := spec.Labels
	if len(names) == 0 {
		names = []string{spec.Name}
	}
	out := make([]scaleset.Label, len(names))
	for i, n := range names {
		out[i] = scaleset.Label{Name: n, Type: "System"}
	}
	return out
}

func sameLabels(a, b []scaleset.Label) bool {
	names := func(ls []scaleset.Label) []string {
		out := make([]string, len(ls))
		for i, l := range ls {
			out[i] = l.Name
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(names(a), names(b))
}

func (c *client) OpenSession(ctx context.Context, scaleSetID int, owner string) (Session, error) {
	var opts []scaleset.HTTPOption
	if c.log != nil && c.log.Enabled(ctx, slog.LevelDebug) {
		rc := retryablehttp.NewClient()
		rc.RetryMax, rc.RetryWaitMax = 4, 30*time.Second // the library's defaults
		rc.ResponseLogHook = logMessageBodies(c.log, scaleSetID)
		opts = append(opts, scaleset.WithRetryableHTTPClint(rc))
	}
	s, err := c.c.MessageSessionClient(ctx, scaleSetID, owner, opts...)
	if err != nil {
		return nil, err
	}
	return &session{s: s}, nil
}

func (c *client) AcquirableJobs(ctx context.Context, scaleSetID int) ([]Job, error) {
	jobs, err := c.c.GetAcquirableJobs(ctx, scaleSetID)
	if err != nil {
		return nil, err
	}
	out := make([]Job, len(jobs))
	for i, j := range jobs {
		out[i] = job(j.JobMessageBase, "")
	}
	return out, nil
}

func (c *client) GenerateJITConfig(ctx context.Context, scaleSetID int, runnerName string) (*JITConfig, error) {
	jit, err := c.c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: runnerName}, scaleSetID)
	if err != nil {
		return nil, withholdJITConfig(err)
	}
	if jit.Runner == nil {
		return nil, errors.New("the JIT config came back without its runner")
	}
	return &JITConfig{RunnerID: int64(jit.Runner.ID), Encoded: jit.EncodedJITConfig}, nil
}

func (c *client) RemoveRunner(ctx context.Context, runnerID int64, name string) error {
	if runnerID == 0 {
		r, err := c.c.GetRunnerByName(ctx, name)
		if err != nil || r == nil {
			return err
		}
		runnerID = int64(r.ID)
	}
	err := c.c.RemoveRunner(ctx, runnerID)
	switch {
	case errors.Is(err, scaleset.RunnerNotFoundError):
		return nil
	case errors.Is(err, scaleset.JobStillRunningError):
		return fmt.Errorf("%w: %w", ErrJobStillRunning, err)
	}
	return err
}

type session struct {
	s *scaleset.MessageSessionClient
}

func (s *session) Statistics() Statistics {
	return statistics(s.s.Session().Statistics)
}

func (s *session) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*Message, error) {
	m, err := s.s.GetMessage(ctx, lastMessageID, maxCapacity)
	if err != nil || m == nil {
		return nil, err
	}
	out := &Message{ID: m.MessageID, Statistics: statistics(m.Statistics)}
	for _, j := range m.JobAvailableMessages {
		out.Available = append(out.Available, job(j.JobMessageBase, ""))
	}
	for _, j := range m.JobAssignedMessages {
		out.Assigned = append(out.Assigned, job(j.JobMessageBase, ""))
	}
	for _, j := range m.JobStartedMessages {
		out.Started = append(out.Started, job(j.JobMessageBase, j.RunnerName))
	}
	for _, j := range m.JobCompletedMessages {
		c := job(j.JobMessageBase, j.RunnerName)
		c.Result = j.Result
		out.Completed = append(out.Completed, c)
	}
	return out, nil
}

func job(b scaleset.JobMessageBase, runner string) Job {
	return Job{RequestID: b.RunnerRequestID, QueueTime: b.QueueTime, RunnerName: runner}
}

func statistics(s *scaleset.RunnerScaleSetStatistic) Statistics {
	if s == nil {
		return Statistics{}
	}
	return Statistics{AvailableJobs: s.TotalAvailableJobs, AssignedJobs: s.TotalAssignedJobs, RunningJobs: s.TotalRunningJobs}
}

func (s *session) DeleteMessage(ctx context.Context, messageID int) error {
	return s.s.DeleteMessage(ctx, messageID)
}

func (s *session) AcquireJobs(ctx context.Context, requestIDs []int64) error {
	_, err := s.s.AcquireJobs(ctx, requestIDs)
	return err
}

func (s *session) Close(ctx context.Context) error {
	return s.s.Close(ctx)
}

// rawBodyMax caps a logged message body; a message holds at most 50 events.
const rawBodyMax = 64 << 10

// logMessageBodies logs the body of each message poll, so the payload's
// actual field names are on record (#12). Only a GET's 200 is logged: the
// session's other calls return its access token.
func logMessageBodies(log *slog.Logger, scaleSetID int) retryablehttp.ResponseLogHook {
	return func(_ retryablehttp.Logger, resp *http.Response) {
		if resp.Request == nil || resp.Request.Method != http.MethodGet || resp.StatusCode != http.StatusOK {
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(b))
		if err != nil {
			return
		}
		body := string(b)
		if len(body) > rawBodyMax {
			body = body[:rawBodyMax] + "…(truncated)"
		}
		log.Debug("raw message", "scale_set_id", scaleSetID, "bytes", len(b), "body", body)
	}
}

// withholdJITConfig keeps runner credentials out of errors. On a decode
// failure actions/scaleset quotes the rest of the body, which on a 200 is
// the JIT config, so any error on a 200 is replaced whole. Refusals carry
// GitHub's message, not credentials, and pass through.
func withholdJITConfig(err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, `status="200 `) || strings.Contains(msg, "decode") || strings.Contains(msg, "encodedjitconfig") {
		return errors.New("GitHub's answer to the JIT config request couldn't be decoded (its body is withheld, since it may hold the runner's credentials)")
	}
	return err
}

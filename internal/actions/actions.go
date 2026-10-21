// Package actions wraps GitHub's runner scale set API.
//
// actions/scaleset is in public preview, so only this package imports it.
// The controller sees Client, Session and the types below, and fakes the two
// interfaces in its tests.
package actions

import (
	"context"
	"errors"
	"time"
)

// Client is the scale set API of one organisation or repository.
type Client interface {
	// EnsureScaleSet returns the scale set's ID, creating it or updating its
	// labels as needed, and which it did.
	EnsureScaleSet(ctx context.Context, spec ScaleSetSpec) (int, Change, error)
	// OpenSession opens the scale set's message session; only one may be
	// open at a time.
	OpenSession(ctx context.Context, scaleSetID int, owner string) (Session, error)
	// AcquirableJobs lists jobs available to the scale set and not yet
	// acquired, including those offered to a session that is gone.
	AcquirableJobs(ctx context.Context, scaleSetID int) ([]Job, error)
	// GenerateJITConfig registers a single-job runner and returns its
	// config.
	GenerateJITConfig(ctx context.Context, scaleSetID int, runnerName string) (*JITConfig, error)
	// RemoveRunner deletes a registration by ID, or by name when the ID is
	// 0 (GenerateJITConfig's answer was lost). Already gone is not an error;
	// a runner with a job yields ErrJobStillRunning.
	RemoveRunner(ctx context.Context, runnerID int64, name string) error
}

// Session is one scale set's long-poll message session.
type Session interface {
	// Statistics are the scale set's statistics when the session opened.
	Statistics() Statistics
	// GetMessage long-polls (~50s) for the message after lastMessageID and
	// returns nil if none arrived. maxCapacity is the most runners the scale
	// set could have at once.
	GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*Message, error)
	// DeleteMessage acknowledges a message.
	DeleteMessage(ctx context.Context, messageID int) error
	// AcquireJobs claims available jobs for the scale set.
	AcquireJobs(ctx context.Context, requestIDs []int64) error
	// Close deletes the session.
	Close(ctx context.Context) error
}

// ErrJobStillRunning: GitHub still counts the runner's job as running. It
// clears once the job ends or GitHub gives up on the runner.
var ErrJobStillRunning = errors.New("the runner's job is still running")

// Change is what EnsureScaleSet did to the scale set.
type Change string

const (
	Unchanged Change = "unchanged"
	Created   Change = "created"
	Updated   Change = "updated"
)

// ScaleSetSpec is a scale set as configured.
type ScaleSetSpec struct {
	Name        string
	RunnerGroup string
	Labels      []string
}

// JITConfig is a registered runner and its config.
type JITConfig struct {
	RunnerID int64
	// Encoded is the value for --jitconfig or ACTIONS_RUNNER_INPUT_JITCONFIG.
	Encoded string
}

// Statistics are GitHub's current counts for a scale set, complete where a
// message's job events (capped at 50) may not be.
type Statistics struct {
	// AvailableJobs counts jobs offered to the scale set and not yet
	// acquired.
	AvailableJobs int
	// AssignedJobs counts jobs assigned, waiting or running: the runners
	// needed before min_runners.
	AssignedJobs int
	// RunningJobs counts assigned jobs a runner has started.
	RunningJobs int
}

// Message is one batch of session events.
type Message struct {
	ID         int
	Statistics Statistics
	// Available jobs must be acquired for the scale set.
	Available []Job
	Assigned  []Job
	Started   []Job
	Completed []Job
}

// Job is one job event.
type Job struct {
	// RequestID identifies the job across events.
	RequestID int64
	// QueueTime is when the job was queued.
	QueueTime time.Time
	// RunnerName is set on started and completed events.
	RunnerName string
	// Result is set on completed events: "succeeded", "canceled", ...
	Result string
}

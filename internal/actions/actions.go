// Package actions is the controller's only view of GitHub's runner scale set
// API.
//
// actions/scaleset is in public preview and its interfaces may change, so
// everything that touches it lives here, behind Client and Session. The rest
// of the controller sees only the types below, and its tests use fakes of
// these two interfaces.
package actions

import (
	"context"
	"errors"
	"time"
)

// Client is what the controller does with one organisation's or one
// repository's scale set API.
type Client interface {
	// EnsureScaleSet returns the ID of the named scale set, creating it, or
	// updating its labels to match, as needed.
	EnsureScaleSet(ctx context.Context, spec ScaleSetSpec) (int, error)
	// OpenSession starts the scale set's message session. Only one can be
	// open per scale set at a time.
	OpenSession(ctx context.Context, scaleSetID int, owner string) (Session, error)
	// GenerateJITConfig registers a runner and returns the configuration
	// that lets it run exactly one job.
	GenerateJITConfig(ctx context.Context, scaleSetID int, runnerName string) (*JITConfig, error)
	// RemoveRunner deletes a runner's registration, by its ID or, when the
	// ID is 0 because GenerateJITConfig's answer never arrived, by its name.
	// A registration that is already gone is not an error. While GitHub
	// still counts the runner's job as running, it returns
	// ErrJobStillRunning.
	RemoveRunner(ctx context.Context, runnerID int64, name string) error
}

// Session is one scale set's long-poll message session.
type Session interface {
	// Statistics are the scale set's statistics when the session opened.
	Statistics() Statistics
	// GetMessage waits up to about 50 seconds for the message after
	// lastMessageID, and returns nil if none arrived. maxCapacity tells
	// GitHub how many runners the scale set could have at once.
	GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*Message, error)
	// DeleteMessage acknowledges a message, so it isn't delivered again.
	DeleteMessage(ctx context.Context, messageID int) error
	// AcquireJobs claims available jobs for the scale set.
	AcquireJobs(ctx context.Context, requestIDs []int64) error
	// Close deletes the session.
	Close(ctx context.Context) error
}

// ErrJobStillRunning is RemoveRunner's error while GitHub still counts the
// runner's job as running. It clears once GitHub gives up on the runner.
var ErrJobStillRunning = errors.New("the runner's job is still running")

// ScaleSetSpec is a scale set as the configuration declares it.
type ScaleSetSpec struct {
	Name        string
	RunnerGroup string
	Labels      []string
}

// JITConfig is a registered runner and the configuration it starts with.
type JITConfig struct {
	RunnerID int64
	// Encoded is the value of the runner's --jitconfig, or of
	// ACTIONS_RUNNER_INPUT_JITCONFIG.
	Encoded string
}

// Statistics are GitHub's counts for a scale set. They are always current,
// unlike a message's job events, which are capped at 50 per message.
type Statistics struct {
	// AssignedJobs are the jobs assigned to the scale set, whether waiting
	// for a runner or running. It is the number of runners the scale set
	// needs.
	AssignedJobs int
	// RunningJobs are the assigned jobs a runner has started.
	RunningJobs int
}

// Message is one batch of a session's events.
type Message struct {
	ID         int
	Statistics Statistics
	// Available jobs are acquired before anything else is done.
	Available []Job
	Assigned  []Job
	Started   []Job
	Completed []Job
}

// Job is one job event.
type Job struct {
	// RequestID identifies the job across its events.
	RequestID int64
	// QueueTime is when the job was queued.
	QueueTime time.Time
	// RunnerName is the runner a started or completed job ran on.
	RunnerName string
	// Result is a completed job's result, such as "succeeded" or "canceled".
	Result string
}

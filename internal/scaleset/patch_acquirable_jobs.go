// Patch to actions/scaleset v0.4.0, not in upstream. The other .go files are
// upstream's, unchanged; see README.md.

package scaleset

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Runs after client.go's init (files initialise in name order), so the user
// agent reports this copy rather than "unknown".
func init() {
	buildInfo = clientBuildInfo{version: "v0.4.0+runner-controller", commitSHA: "patched"}
}

type acquirableJobsResponse struct {
	Count int            `json:"count"`
	Value []JobAvailable `json:"value"`
}

// GetAcquirableJobs lists the jobs available to a runner scale set and not
// yet acquired. A JobAvailable message goes only to the session open at the
// time, so a job that became available while no session was open is offered
// again only with the scale set's next message. Listing them when a session
// opens lets them be acquired at once. actions-runner-controller's original
// listener did the same.
func (c *Client) GetAcquirableJobs(ctx context.Context, runnerScaleSetID int) ([]JobAvailable, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := fmt.Sprintf("/%s/%d/acquirablejobs", scaleSetEndpoint, runnerScaleSetID)
	req, err := c.newActionsServiceRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create new actions service request: %w", err)
	}

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to issue the request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, newRequestResponseError(req, resp, fmt.Errorf("unexpected status code: %d", resp.StatusCode))
	}

	var list acquirableJobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, newRequestResponseError(req, resp, fmt.Errorf("failed to decode acquirable jobs: %w", err))
	}
	return list.Value, nil
}

package threads

import (
	"encoding/json"
	"net/url"
	"os"
	"sync"
)

// capturedRequest mirrors one entry of the JSON array the companion Node
// capture step (THREADS_CAPTURE_GRAPHQL=1 in the threads-toolkit repo) writes
// to disk: every GraphQL POST a real, logged-in browser session issued while
// scrolling a profile page.
type capturedRequest struct {
	URL      string            `json:"url"`
	PostData string            `json:"postData"`
	Headers  map[string]string `json:"headers"`
}

// graphqlTemplate is the piece of a capturedRequest this client actually
// needs to replay pagination: the doc_id and the full relay variable set a
// live page sent. Threads' persisted queries validate the whole variable set
// server-side - the minimal handful this client used to send (after, before,
// first, last, userID, five relay flags) increasingly gets a flat
// {"errors":[{"message":"execution error"...}]} instead of data, even though
// the request is otherwise well-formed. Replaying the exact variable set a
// real page sent (~35 relay flags) is what makes it execute.
type graphqlTemplate struct {
	docID     string
	variables map[string]any
}

const profileThreadsFriendlyName = "BarcelonaProfileThreadsTabRefetchableDirectQuery"

// loadCaptureTemplate reads path (a captured_graphql.json produced by the
// Node capture step) and pulls out the profile-threads pagination request.
func loadCaptureTemplate(path string) (*graphqlTemplate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var requests []capturedRequest
	if err := json.Unmarshal(raw, &requests); err != nil {
		return nil, codeErr(ExitUsage, "capture file %s is not valid JSON: %v", path, err)
	}
	for _, r := range requests {
		if r.Headers["x-fb-friendly-name"] != profileThreadsFriendlyName {
			continue
		}
		form, err := url.ParseQuery(r.PostData)
		if err != nil {
			continue
		}
		docID := form.Get("doc_id")
		if docID == "" {
			continue
		}
		var vars map[string]any
		if err := json.Unmarshal([]byte(form.Get("variables")), &vars); err != nil {
			continue
		}
		return &graphqlTemplate{docID: docID, variables: vars}, nil
	}
	return nil, codeErr(ExitUsage, "no %s request found in %s - re-run the capture step", profileThreadsFriendlyName, path)
}

// captureTemplate lazily loads and caches c.cfg.CaptureFile for the life of
// the client. A missing/unset file is not an error: callers fall back to the
// built-in DocID constants and minimal variable set.
func (c *Client) captureTemplate() *graphqlTemplate {
	c.captureOnce.Do(func() {
		if c.cfg.CaptureFile == "" {
			return
		}
		tpl, err := loadCaptureTemplate(c.cfg.CaptureFile)
		if err != nil {
			c.logf(1, "capture file: %v", err)
			return
		}
		c.captureTpl = tpl
	})
	return c.captureTpl
}

// captureState is embedded via the fields below; kept in its own file next to
// the loader for locality.
type captureState struct {
	captureOnce sync.Once
	captureTpl  *graphqlTemplate
}

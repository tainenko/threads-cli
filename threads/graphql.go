package threads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The GraphQL path. Threads marks a caller as a crawler through a set of
// relay provider flags; with those set, the persisted profile-threads, post,
// and search queries return data without a session. doc_id values rotate
// (see config.go), so a stale id used to degrade to "no extra data" - Threads
// has since tightened this so a variable set the current doc_id doesn't
// recognize gets a flat {"errors":[...]} instead, which graphqlPost treats as
// an error rather than "no more results" (see the env.Data handling below).

// relayProviderVars is the minimal, anonymous-crawler flag set: enough for
// the SSR-adjacent logged-out queries this client was originally built
// around. It is deliberately NOT merged into a captured template's variables
// (see graphqlProfileThreads) - the template already carries a real session's
// full flag set, and overwriting it with these anonymous defaults would just
// reintroduce the "execution error" failure mode captureTemplate exists to
// avoid.
func relayProviderVars() map[string]any {
	return map[string]any{
		"__relay_internal__pv__BarcelonaIsLoggedInrelayprovider":             false,
		"__relay_internal__pv__BarcelonaIsInternalUserrelayprovider":         false,
		"__relay_internal__pv__BarcelonaIsCrawlerrelayprovider":              true,
		"__relay_internal__pv__BarcelonaOptionalCookiesEnabledrelayprovider": true,
		"__relay_internal__pv__BarcelonaIsLoggedOutrelayprovider":            true,
	}
}

// maxGraphQLPages caps how far pagination walks, so an unbounded crawl cannot
// loop forever if Threads' has_next_page ever gets stuck true. A real full
// history backfill (a ~2 year old, active account) has been observed to take
// ~110 pages at 10 posts/page; 500 leaves ample headroom.
const maxGraphQLPages = 500

// graphqlProfileThreads walks a user's posts via the persisted profile-threads
// query, following the page_info cursor from startCursor until it runs out or
// the page cap is hit. startCursor is the end_cursor from the server-rendered
// window, so pagination resumes where the SSR page left off.
//
// When c.cfg.CaptureFile is set (see graphql_capture.go), every request
// reuses the exact doc_id and full relay variable set a real logged-in
// browser session sent - only "after" and "userID" are overridden per page.
// This is what actually unlocks a full history: Threads visibly caps
// anonymous/under-authenticated profile pagination at roughly 20 posts
// regardless of doc_id freshness, but a real session's variable set does not
// hit that ceiling. Without a capture file, this falls back to
// DocIDProfileThreads and the minimal anonymous variable set - subject to
// both that ceiling and to doc_id going stale until the constant is updated.
func (c *Client) graphqlProfileThreads(ctx context.Context, userID, startCursor string) ([]Post, error) {
	tpl := c.captureTemplate()

	var out []Post
	cursor := startCursor
	for page := 0; page < maxGraphQLPages; page++ {
		docID := DocIDProfileThreads
		var vars map[string]any
		if tpl != nil {
			docID = tpl.docID
			vars = cloneVars(tpl.variables)
		} else {
			vars = relayProviderVars()
		}
		vars["userID"] = userID
		if cursor != "" {
			vars["after"] = cursor
		} else {
			vars["after"] = nil
		}

		raw, err := c.graphqlPost(ctx, docID, vars)
		if err != nil {
			return out, err
		}
		posts := postsFromGraphQL(raw)
		out = append(out, posts...)
		next, more, ok := findPageInfo(raw, 0)
		if !ok || !more || next == "" || next == cursor {
			break
		}
		if len(posts) == 0 {
			// Cursor advanced but nothing came back with it - stop rather
			// than spin for maxGraphQLPages requests on a dead end.
			break
		}
		cursor = next
	}
	return out, nil
}

// cloneVars shallow-copies a variables map so per-page overrides (after,
// userID) never mutate the cached template between pages or requests.
func cloneVars(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// graphqlPostReplies fetches a window of a post's replies via the persisted
// query.
func (c *Client) graphqlPostReplies(ctx context.Context, postID string) ([]Post, error) {
	vars := relayProviderVars()
	vars["postID"] = postID
	raw, err := c.graphqlPost(ctx, DocIDPostPage, vars)
	if err != nil {
		return nil, err
	}
	return postsFromGraphQL(raw), nil
}

// graphqlSearch runs the keyword search persisted query.
func (c *Client) graphqlSearch(ctx context.Context, query string) ([]Post, error) {
	vars := relayProviderVars()
	vars["query"] = query
	raw, err := c.graphqlPost(ctx, DocIDSearch, vars)
	if err != nil {
		return nil, err
	}
	return postsFromGraphQL(raw), nil
}

// graphqlPost POSTs a persisted query and returns the decoded data tree. A
// response with no "data" field - Threads' shape for a doc_id that rejected
// the variable set outright, as well as for a genuinely malformed request -
// surfaces as the same "unexpected shape" CodeError either way; callers don't
// need to tell the two apart, both mean "stop and refresh the capture file."
func (c *Client) graphqlPost(ctx context.Context, docID string, vars map[string]any) (any, error) {
	varsJSON, _ := json.Marshal(vars)
	form := url.Values{}
	form.Set("lsd", "t")
	form.Set("doc_id", docID)
	form.Set("variables", string(varsJSON))

	c.rateLimit()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GraphQLURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-FB-LSD", "t")
	req.Header.Set("X-IG-App-ID", "238260118697367")
	if c.cfg.Session != "" {
		req.Header.Set("Cookie", "sessionid="+c.cfg.Session)
	}
	if c.cfg.CSRF != "" {
		req.Header.Set("X-CSRFToken", c.cfg.CSRF)
	}
	c.logf(2, "POST graphql doc_id=%s", docID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, codeErr(ExitNetwork, "graphql request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Data) == 0 {
		return nil, codeErr(ExitNotFound, "graphql returned no data (doc_id may be stale - see THREADS_CAPTURE_FILE)")
	}
	var data any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, codeErr(ExitNotFound, "graphql returned an unexpected shape (doc_id may be stale)")
	}
	return data, nil
}

// postsFromGraphQL reuses the SSR thread_items walker over the GraphQL data
// tree: both surfaces nest the same post objects.
func postsFromGraphQL(data any) []Post {
	posts := walkThreadItems(data, 0)
	out := posts[:0]
	seen := map[string]bool{}
	for _, p := range posts {
		if p.ID == "" || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	return out
}

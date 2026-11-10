package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ghCall is one recorded GitHub API request.
type ghCall struct {
	method, path, token string
	body                []byte
}

// ghStub installs a gh seam that records calls and answers with reply(call).
func ghStub(t *testing.T, reply func(c ghCall) ([]byte, error)) *[]ghCall {
	t.Helper()
	var mu sync.Mutex
	var calls []ghCall
	swap(t, &gh, func(method, path, token string, body []byte) ([]byte, error) {
		c := ghCall{method, path, token, body}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		return reply(c)
	})
	return &calls
}

func TestRunnerGroupIDReturnsConfiguredIDForUserTenant(t *testing.T) {
	swap(t, &targetKind, "user")
	swap(t, &runnerGroupDefault, 1)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return nil, errors.New("no API call expected") })
	if id, err := runnerGroupID("tok", "build-ephemeral"); err != nil || id != 1 {
		t.Errorf("got (%d, %v), want (1, nil)", id, err)
	}
	if len(*calls) != 0 {
		t.Errorf("user tenant must not query runner groups, got %v", *calls)
	}
}

func TestGroupIDCacheKeyedByTargetAndName(t *testing.T) {
	freshState(t)
	swap(t, &targetKind, "org")
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		id := 77
		if strings.Split(c.path, "/")[2] == "alpha" {
			id = 42
		}
		return groupsReply(t, "build-ephemeral", id), nil
	})
	for _, want := range []struct {
		target string
		id     int
	}{{"alpha", 42}, {"beta", 77}, {"alpha", 42}} {
		swap(t, &target, want.target)
		if id, err := runnerGroupID("tok", "build-ephemeral"); err != nil || id != want.id {
			t.Errorf("%s = (%d, %v), want %d", want.target, id, err, want.id)
		}
	}
	if len(*calls) != 2 {
		t.Errorf("third call must be served from cache: got %d fetches, want 2", len(*calls))
	}
}

// The cache was a bare map written by every spawn goroutine; a concurrent map
// write is a fatal runtime error that no recover() catches, so one burst of
// queued jobs on a cold cache killed the whole broker. Run under -race.
func TestRunnerGroupIDIsSafeForConcurrentCalls(t *testing.T) {
	freshState(t)
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	var fetches atomic.Int32
	ghStub(t, func(ghCall) ([]byte, error) {
		fetches.Add(1)
		time.Sleep(5 * time.Millisecond) // widen the miss window so goroutines overlap
		return groupsReply(t, "build-ephemeral", 42), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := runnerGroupID("tok", "build-ephemeral"); err != nil || id != 42 {
				t.Errorf("got (%d, %v), want (42, nil)", id, err)
			}
		}()
	}
	wg.Wait()
	if n := fetches.Load(); n != 1 {
		t.Errorf("a cold cache hit by 64 jobs should fetch once, fetched %d times", n)
	}
}

// Python indexed match["id"] and raised KeyError; Go read a float64 with `, _`
// and cached group id 0 for the life of the process.
func TestRunnerGroupIDRejectsAGroupWithoutAPositiveID(t *testing.T) {
	for name, reply := range map[string]string{
		"missing id": `{"runner_groups":[{"name":"build-ephemeral"}]}`,
		"zero id":    `{"runner_groups":[{"name":"build-ephemeral","id":0}]}`,
		"negative":   `{"runner_groups":[{"name":"build-ephemeral","id":-3}]}`,
		"null id":    `{"runner_groups":[{"name":"build-ephemeral","id":null}]}`,
		"string id":  `{"runner_groups":[{"name":"build-ephemeral","id":"7"}]}`,
		"fractional": `{"runner_groups":[{"name":"build-ephemeral","id":1.5}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			freshState(t)
			swap(t, &targetKind, "org")
			swap(t, &target, "alpha-org")
			ghStub(t, func(ghCall) ([]byte, error) { return []byte(reply), nil })
			if id, err := runnerGroupID("tok", "build-ephemeral"); err == nil {
				t.Fatalf("got id %d, want an error", id)
			}
			if len(groupIDCache) != 0 {
				t.Errorf("a bad reply must not be cached: %v", groupIDCache)
			}
		})
	}
}

func TestRunnerGroupIDErrorsWhenTheGroupIsNotListed(t *testing.T) {
	for name, reply := range map[string]string{
		"other group":      `{"runner_groups":[{"name":"other","id":9}]}`,
		"no groups key":    `{}`,
		"empty body":       ``,
		"not json":         `<html>`,
		"groups not array": `{"runner_groups":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			freshState(t)
			swap(t, &targetKind, "org")
			swap(t, &target, "alpha-org")
			ghStub(t, func(ghCall) ([]byte, error) { return []byte(reply), nil })
			if _, err := runnerGroupID("tok", "build-ephemeral"); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// jitBody is the request GitHub receives, decoded back out of the wire bytes.
type jitBody struct {
	Name          string   `json:"name"`
	RunnerGroupID int      `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder"`
}

func lastJitPost(t *testing.T, calls []ghCall) (ghCall, jitBody) {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].method == "POST" {
			var b jitBody
			if err := json.Unmarshal(calls[i].body, &b); err != nil {
				t.Fatalf("request body is not the jitconfig request: %v", err)
			}
			return calls[i], b
		}
	}
	t.Fatal("no POST was made")
	return ghCall{}, jitBody{}
}

func TestMintPostsToOrgPath(t *testing.T) {
	freshState(t)
	swap(t, &tenant, "alpha-org")
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "build-ephemeral", 42), nil
		}
		return []byte(`{"encoded_jit_config":"JIT"}`), nil
	})
	got, err := mintJitconfig("tok", "ci-build-1-aaa", "build", tiersFixture, "alpha-org/app")
	if err != nil || got != "JIT" {
		t.Fatalf("got (%q, %v)", got, err)
	}
	post, body := lastJitPost(t, *calls)
	if post.path != "/orgs/alpha-org/actions/runners/generate-jitconfig" || post.token != "tok" {
		t.Errorf("post = %+v", post)
	}
	want := jitBody{Name: "ci-build-1-aaa", RunnerGroupID: 42, Labels: []string{"self-hosted", "alpha-org", "build"}, WorkFolder: "_work"}
	if body.Name != want.Name || body.RunnerGroupID != want.RunnerGroupID || body.WorkFolder != want.WorkFolder ||
		strings.Join(body.Labels, ",") != strings.Join(want.Labels, ",") {
		t.Errorf("body = %+v, want %+v", body, want)
	}
}

// C5: the labels are the fixed triple, and the infra tier resolves its own
// group (mirrors test_mint_jitconfig_mints_self_hosted_tenant_and_tier_only).
func TestMintMintsSelfHostedTenantAndTierOnly(t *testing.T) {
	freshState(t)
	swap(t, &tenant, "alpha-org")
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "infra-ephemeral", 99), nil
		}
		return []byte(`{"encoded_jit_config":"fake-jit"}`), nil
	})
	got, err := mintJitconfig("tok", "ci-infra-123-abcdef", "infra", tiersFixture, "alpha-org/platform")
	if err != nil || got != "fake-jit" {
		t.Fatalf("got (%q, %v)", got, err)
	}
	_, body := lastJitPost(t, *calls)
	if strings.Join(body.Labels, ",") != "self-hosted,alpha-org,infra" || body.RunnerGroupID != 99 {
		t.Errorf("body = %+v", body)
	}
}

func TestMintPostsToRepoPathForUserTenant(t *testing.T) {
	freshState(t)
	swap(t, &tenant, "juno")
	swap(t, &targetKind, "user")
	swap(t, &target, "Juno")
	swap(t, &tenantOwner, "juno")
	swap(t, &repos, []string{"some-repo"})
	swap(t, &runnerGroupDefault, 1)
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			t.Error("runner-groups must not be queried for a user tenant")
		}
		return []byte(`{"encoded_jit_config":"JIT"}`), nil
	})
	got, err := mintJitconfig("tok", "ci-build-9-zzz", "build", tiersFixture, "Juno/some-repo")
	if err != nil || got != "JIT" {
		t.Fatalf("got (%q, %v)", got, err)
	}
	post, body := lastJitPost(t, *calls)
	if post.path != "/repos/Juno/some-repo/actions/runners/generate-jitconfig" {
		t.Errorf("path = %q", post.path)
	}
	if body.RunnerGroupID != 1 || strings.Join(body.Labels, ",") != "self-hosted,juno,build" {
		t.Errorf("body = %+v", body)
	}
}

// Python indexed resp["encoded_jit_config"] (KeyError); Go returned ("", nil)
// and launched a VM that could never register.
func TestMintRejectsAReplyWithoutAJitconfig(t *testing.T) {
	for name, reply := range map[string]string{
		"empty object": `{}`,
		"empty body":   ``,
		"empty string": `{"encoded_jit_config":""}`,
		"null":         `{"encoded_jit_config":null}`,
		"json null":    `null`,
		"wrong type":   `{"encoded_jit_config":5}`,
		"array":        `[]`,
		"not json":     `<html>`,
	} {
		t.Run(name, func(t *testing.T) {
			freshState(t)
			swap(t, &targetKind, "user")
			swap(t, &target, "Juno")
			swap(t, &tenantOwner, "juno")
			swap(t, &repos, []string{"some-repo"})
			ghStub(t, func(ghCall) ([]byte, error) { return []byte(reply), nil })
			if got, err := mintJitconfig("tok", "ci-build-1-aaa", "build", tiersFixture, "Juno/some-repo"); err == nil {
				t.Fatalf("got %q, want an error", got)
			}
		})
	}
}

func TestMintPropagatesGitHubErrors(t *testing.T) {
	freshState(t)
	swap(t, &targetKind, "user")
	swap(t, &target, "Juno")
	swap(t, &tenantOwner, "juno")
	swap(t, &repos, []string{"some-repo"})
	ghStub(t, func(ghCall) ([]byte, error) { return nil, &httpError{code: 403, body: "nope"} })
	if _, err := mintJitconfig("tok", "n", "build", tiersFixture, "Juno/some-repo"); err == nil {
		t.Fatal("expected the GitHub error")
	}
}

func TestMintFailsClosedForAnUnregisteredUserRepoBeforeAnyAPICall(t *testing.T) {
	freshState(t)
	swap(t, &targetKind, "user")
	swap(t, &target, "Juno")
	swap(t, &tenantOwner, "juno")
	swap(t, &repos, []string{"some-repo"})
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{"encoded_jit_config":"JIT"}`), nil })
	if _, err := mintJitconfig("tok", "n", "build", tiersFixture, "Juno/other"); err == nil {
		t.Fatal("expected an error")
	}
	if len(*calls) != 0 {
		t.Errorf("no API call for an unregistered repo, got %v", *calls)
	}
}

func TestCancelJobSwallowsHTTPError(t *testing.T) {
	logs := captureLog(t)
	ghStub(t, func(ghCall) ([]byte, error) { return nil, &httpError{code: 422, body: "already cancelled"} })
	cancelJob("tok", "alpha-org/platform", 123) // must not panic or return an error
	if !strings.Contains(logs.String(), "cancel failed for run 123") {
		t.Errorf("failure not logged: %q", logs.String())
	}
}

func TestCancelJobPostsTheRunCancelPath(t *testing.T) {
	calls := ghStub(t, func(ghCall) ([]byte, error) { return nil, nil })
	cancelJob("tok", "alpha-org/some-repo", 999)
	if len(*calls) != 1 {
		t.Fatalf("calls = %v", *calls)
	}
	if c := (*calls)[0]; c.method != "POST" || c.path != "/repos/alpha-org/some-repo/actions/runs/999/cancel" || c.token != "tok" {
		t.Errorf("call = %+v", c)
	}
}

func TestGHSendsAnAuthenticatedRequestAndReturnsTheBody(t *testing.T) {
	var gotHdr http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHdr = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	swap(t, &ghAPI, srv.URL)
	out, err := ghHTTP("POST", "/x", "tok", []byte(`{"a":1}`))
	if err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("got (%q, %v)", out, err)
	}
	if gotHdr.Get("Authorization") != "Bearer tok" || gotHdr.Get("Accept") != "application/vnd.github+json" ||
		gotHdr.Get("X-GitHub-Api-Version") != "2022-11-28" || gotHdr.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", gotHdr)
	}
	if string(gotBody) != `{"a":1}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestGHReturnsAnHTTPErrorForFailureStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("line1\nforged log line"))
	}))
	defer srv.Close()
	swap(t, &ghAPI, srv.URL)
	_, err := ghHTTP("POST", "/x", "tok", nil)
	var he *httpError
	if !errors.As(err, &he) || he.code != 422 {
		t.Fatalf("err = %v, want *httpError 422", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("a GitHub error body must not break the log line: %q", err.Error())
	}
}

// repository.full_name is GitHub-generated, but the label_vocabulary deny
// cancels before any owner check, so a crafted name would otherwise steer the
// installation token's API call to another path.
func TestCancelJobRefusesAMalformedRepoName(t *testing.T) {
	for _, bad := range []string{
		"a/b/../../../orgs/victim/x?y=1#frag", "no-slash", "/repo", "owner/", "a/b/c", "a/..", "a/.", "../b",
		"a b/c", "a/b\nforged", "", "-a/b", "a/b?x", "a/b#x", "a%2fb/c",
	} {
		t.Run(bad, func(t *testing.T) {
			logs := captureLog(t)
			calls := ghStub(t, func(ghCall) ([]byte, error) { return nil, nil })
			cancelJob("tok", bad, 5)
			if len(*calls) != 0 {
				t.Errorf("a malformed repo name reached the API: %v", ghPaths(*calls))
			}
			if !strings.Contains(logs.String(), "cancel skipped") || strings.Count(logs.String(), "\n") != 1 {
				t.Errorf("want one 'cancel skipped' line, got %q", logs.String())
			}
		})
	}
	for _, good := range []string{"alpha-org/some-repo", "Juno/Some-Repo.v2_x", "a/b", "a-b/.github"} {
		calls := ghStub(t, func(ghCall) ([]byte, error) { return nil, nil })
		cancelJob("tok", good, 5)
		if len(*calls) != 1 {
			t.Errorf("%q must be cancelled normally", good)
		}
	}
}

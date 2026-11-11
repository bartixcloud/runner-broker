package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

// swap sets *p to v and restores the old value when the test ends, so no test
// inherits another's stubs (a leaked gh stub or target made tests
// order-dependent).
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// freshState gives a test its own reservation map, launch claims and
// runner-group cache.
func freshState(t *testing.T) {
	t.Helper()
	swap(t, &reserved, map[string]int{})
	swap(t, &claimed, map[int]string{})
	swap(t, &groupIDCache, map[[2]string]int{})
}

// captureLog redirects the standard logger into a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldW, oldF := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldW); log.SetFlags(oldF) })
	return &buf
}

// mustJSON marshals v for use as a stubbed GitHub reply.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// groupsReply is a runner-groups reply holding one group.
func groupsReply(t *testing.T, name string, id int) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{"runner_groups": []any{map[string]any{"name": name, "id": id}}})
}

// lines splits log or script output into its non-empty lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

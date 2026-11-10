package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

const testToken = "123456:TEST-token_abc"

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telegram-token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadTelegramUnsetMeansNoNotifier(t *testing.T) {
	n, err := LoadTelegram(env(nil))
	if n != nil || err != nil {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestLoadTelegramReadsTokenFileAndChat(t *testing.T) {
	file := writeTokenFile(t, testToken+"\n")
	n, err := LoadTelegram(env(map[string]string{envTelegramTokenFile: file, envTelegramChatID: "-1001234567890"}))
	if err != nil || n == nil {
		t.Fatalf("got %v, %v", n, err)
	}
	if n.Token != testToken || n.ChatID != "-1001234567890" || n.APIBase != telegramAPIBase {
		t.Errorf("notifier = %+v", n)
	}
}

func TestLoadTelegramRejectsHalfConfigurationAndBadValues(t *testing.T) {
	file := writeTokenFile(t, testToken)
	empty := writeTokenFile(t, "  \n")
	spaced := writeTokenFile(t, "abc def")
	for name, e := range map[string]map[string]string{
		"only file":    {envTelegramTokenFile: file},
		"only chat":    {envTelegramChatID: "12345"},
		"relative":     {envTelegramTokenFile: "token", envTelegramChatID: "12345"},
		"bad chat":     {envTelegramTokenFile: file, envTelegramChatID: "abc; rm"},
		"missing file": {envTelegramTokenFile: filepath.Join(t.TempDir(), "none"), envTelegramChatID: "12345"},
		"empty token":  {envTelegramTokenFile: empty, envTelegramChatID: "12345"},
		"token spaces": {envTelegramTokenFile: spaced, envTelegramChatID: "12345"},
	} {
		if n, err := LoadTelegram(env(e)); err == nil || n != nil {
			t.Errorf("%s: got %v, %v", name, n, err)
		}
	}
}

func TestLoadTelegramErrorsNeverCarryTheToken(t *testing.T) {
	file := writeTokenFile(t, testToken+" x")
	_, err := LoadTelegram(env(map[string]string{envTelegramTokenFile: file, envTelegramChatID: "12345"}))
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestTelegramNotifyPostsOneMessageForAllFindings(t *testing.T) {
	var (
		mu       sync.Mutex
		gotPath  string
		gotType  string
		gotBody  map[string]any
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	n := &TelegramNotifier{APIBase: srv.URL, Token: testToken, ChatID: "42", Client: srv.Client()}
	ref := "refs/heads/main"
	err := n.Notify(context.Background(), []Befund{
		{Art: ArtForcePush, Repo: "demo", Ref: &ref, Schwere: SchwereKritisch, Grund: "Force-Push auf refs/heads/main"},
		{Art: ArtUnpruefbar, Repo: "demo", Schwere: SchwereWarnung, Grund: "restic-Snapshot fehlgeschlagen"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 || gotPath != "/bot"+testToken+"/sendMessage" || gotType != "application/json" {
		t.Errorf("requests=%d path=%q type=%q", requests, gotPath, gotType)
	}
	if gotBody["chat_id"] != "42" {
		t.Errorf("chat_id = %v", gotBody["chat_id"])
	}
	text, _ := gotBody["text"].(string)
	for _, want := range []string{
		"2 Befund(e)",
		"[kritisch] force_push demo refs/heads/main: Force-Push auf refs/heads/main",
		"[warnung] unpruefbar demo: restic-Snapshot fehlgeschlagen",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestTelegramNotifyWithoutFindingsSendsNothing(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	n := &TelegramNotifier{APIBase: srv.URL, Token: testToken, ChatID: "42", Client: srv.Client()}
	if err := n.Notify(context.Background(), nil); err != nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestTelegramNotifyReportsHTTPFailureWithoutTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	defer srv.Close()
	n := &TelegramNotifier{APIBase: srv.URL, Token: testToken, ChatID: "42", Client: srv.Client()}
	err := n.Notify(context.Background(), []Befund{{Art: ArtUnpruefbar, Repo: "demo", Schwere: SchwereWarnung, Grund: "x"}})
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestTelegramNotifyTransportErrorNeverCarriesTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing listens any more: the dial fails and net/http wraps the URL into the error
	n := &TelegramNotifier{APIBase: base, Token: testToken, ChatID: "42", Client: &http.Client{}}
	err := n.Notify(context.Background(), []Befund{{Art: ArtUnpruefbar, Repo: "demo", Schwere: SchwereWarnung, Grund: "x"}})
	if err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "/bot") {
		t.Fatalf("err = %v", err)
	}
}

func TestTelegramTextIsClippedBelowTheMessageLimit(t *testing.T) {
	var bs []Befund
	for range 200 {
		bs = append(bs, Befund{Art: ArtUnpruefbar, Repo: "demo", Schwere: SchwereWarnung, Grund: strings.Repeat("g", 100)})
	}
	text := telegramText(bs)
	if len(text) > 4096 || !strings.HasSuffix(text, "(gekürzt)") {
		t.Errorf("len=%d tail=%q", len(text), text[len(text)-20:])
	}
}

// ---------------------------------------------------------------- integration --

type recordingNotifier struct {
	events *orgEvents
	err    error
	mu     sync.Mutex
	got    [][]Art
}

func (r *recordingNotifier) Notify(_ context.Context, bs []Befund) error {
	r.events.add("notify")
	r.mu.Lock()
	r.got = append(r.got, arten(bs))
	r.mu.Unlock()
	return r.err
}

func TestOrgRunRecordsAndAnnouncesTheFindingBeforeRestic(t *testing.T) {
	r := newOrgRig(t)
	rec := &recordingNotifier{events: r.events}
	r.runner.Notify = rec
	var fileDuringRestic []Art
	r.out.onCall = func(c Command) {
		if strings.HasSuffix(c.Argv[0], "restic-sichern.sh") {
			fileDuringRestic = arten(r.befunde())
		}
	}
	body := withFields(t, fixture(t, "push_force.json"), map[string]string{
		"repository": `{"name":"demo","full_name":"ekd-akd/demo","default_branch":"main"}`})
	r.handle("push", body)

	if !slices.Equal(fileDuringRestic, []Art{ArtForcePush}) {
		t.Errorf("the befund file while restic ran = %v, want the force push already there", fileDuringRestic)
	}
	got := r.events.all()
	iNotify, iRestic := slices.Index(got, "notify"), slices.Index(got, "restic-sichern.sh demo")
	if iNotify < 0 || iRestic < 0 || iNotify > iRestic {
		t.Errorf("order = %v, want notify before restic", got)
	}
}

func TestOrgRunAnnouncesAFailedResticSnapshotAsItsOwnFinding(t *testing.T) {
	r := newOrgRig(t)
	rec := &recordingNotifier{events: r.events}
	r.runner.Notify = rec
	r.out.exit = map[string]int{"restic-sichern.sh": 5}
	body := withFields(t, fixture(t, "push_force.json"), map[string]string{
		"repository": `{"name":"demo","full_name":"ekd-akd/demo","default_branch":"main"}`})
	r.handle("push", body)

	if len(rec.got) != 2 || !slices.Equal(rec.got[0], []Art{ArtForcePush}) || !slices.Equal(rec.got[1], []Art{ArtUnpruefbar}) {
		t.Errorf("notifications = %v", rec.got)
	}
	if got := arten(r.befunde()); !slices.Equal(got, []Art{ArtForcePush, ArtUnpruefbar}) {
		t.Errorf("file = %v", got)
	}
}

func TestOrgRunStaysQuietWhenThereIsNothingToReport(t *testing.T) {
	r := newOrgRig(t)
	rec := &recordingNotifier{events: r.events}
	r.runner.Notify = rec
	r.handle("push", fixture(t, "push_normal.json"))
	if len(rec.got) != 0 {
		t.Errorf("notifications = %v", rec.got)
	}
}

func TestOrgRunKeepsTheFileAndTheMirrorWhenNotifyingFails(t *testing.T) {
	r := newOrgRig(t)
	r.runner.Notify = &recordingNotifier{events: r.events, err: errors.New("telegram: status 502")}
	body := withFields(t, fixture(t, "push_force.json"), map[string]string{
		"repository": `{"name":"demo","full_name":"ekd-akd/demo","default_branch":"main"}`})
	r.handle("push", body)
	if got := arten(r.befunde()); !slices.Equal(got, []Art{ArtForcePush}) {
		t.Errorf("file = %v", got)
	}
	if !strings.Contains(r.logs.String(), "notifying about befunde failed") {
		t.Errorf("a failed notification must be logged:\n%s", r.logs.String())
	}
	if got := r.events.all(); got[len(got)-1] != "dispatch fakten.yml demo" {
		t.Errorf("the mirror did not run last: %v", got)
	}
}

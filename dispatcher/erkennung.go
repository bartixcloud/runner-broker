package dispatcher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Port of infra-l1 host/sicherung/erkennung.py (spec K8). The Python file is the
// reference and testdata/sicherung/*.golden.jsonl its output: the identifiers of
// the Befund wire format (art, schwere, the German reasons) are part of that
// interface and stay German on purpose.
//
// Guideline carried over: what cannot be checked is reported as unpruefbar. "No
// finding" means only "known harmless" or "not an event this detection covers",
// never "could not read it". A broken payload must neither stay silent nor panic.

const (
	// maxCommitsImPayload is how many commits GitHub puts into push.commits at most,
	// without saying how many there were in total.
	maxCommitsImPayload = 20

	// unbekanntesRepo names the repo of a Befund whose payload gave none.
	unbekanntesRepo = "unbekannt"

	defaultGeloeschteDateien = 50
	defaultGeloeschteZeilen  = 5000
)

// Art is the closed set of finding kinds.
type Art string

// The kinds of Befund, as written to the befund file.
const (
	ArtForcePush       Art = "force_push"
	ArtRefGeloescht    Art = "ref_geloescht"
	ArtRepoGeloescht   Art = "repo_geloescht"
	ArtRepoUebertragen Art = "repo_uebertragen"
	ArtRepoUmbenannt   Art = "repo_umbenannt"
	ArtRepoOeffentlich Art = "repo_oeffentlich"
	ArtMassenaenderung Art = "massenaenderung"
	ArtUnpruefbar      Art = "unpruefbar"
)

// Schwere is the severity of a Befund; only kritisch triggers a restic snapshot.
type Schwere string

// The severities.
const (
	SchwereKritisch Schwere = "kritisch"
	SchwereWarnung  Schwere = "warnung"
)

// Befund is one detected (or unverifiable) destructive pattern. The field order is
// the order Befund.model_dump_json() writes; Ref is null when the event has none.
type Befund struct {
	Art     Art     `json:"art"`
	Repo    string  `json:"repo"`
	Ref     *string `json:"ref"`
	Schwere Schwere `json:"schwere"`
	Grund   string  `json:"grund"`
}

// Schwellen says from when a deletion counts as a mass change ("greater than", not
// "equal"). The 50 / 5000 default is a guess awaiting the owner's decision (spec O9).
type Schwellen struct {
	GeloeschteDateien int
	GeloeschteZeilen  int
}

// DefaultSchwellen is the reference's default.
func DefaultSchwellen() Schwellen {
	return Schwellen{GeloeschteDateien: defaultGeloeschteDateien, GeloeschteZeilen: defaultGeloeschteZeilen}
}

// DiffStat is what a push deleted between before and after, measured in the backup clone.
type DiffStat struct {
	DateienGeloescht int
	ZeilenGeloescht  int
}

// EncodeBefunde renders one JSON object per line exactly like the reference's
// model_dump_json: compact, UTF-8 unescaped, no HTML escaping.
func EncodeBefunde(befunde []Befund) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // Encode ends every value with a newline
	enc.SetEscapeHTML(false)
	for _, b := range befunde {
		if err := enc.Encode(b); err != nil {
			return nil, fmt.Errorf("encode befund: %w", err)
		}
	}
	return buf.Bytes(), nil
}

func unpruefbar(repo string, ref *string, grund string) Befund {
	return Befund{Art: ArtUnpruefbar, Repo: repo, Ref: ref, Schwere: SchwereWarnung, Grund: grund}
}

func strRef(s string) *string { return &s }

// Bewerte evaluates one webhook event; event is the X-GitHub-Event value. Events
// the detection does not cover yield no findings, even with an unreadable payload.
func Bewerte(event string, payload []byte, s Schwellen, d *DiffStat) []Befund {
	switch event {
	case "push", "delete", "repository":
	default:
		return nil
	}
	var (
		befunde []Befund
		errs    pathErrors
	)
	root, ok := decodeObject(payload)
	if !ok {
		return []Befund{ungueltig(payload, event, nil)}
	}
	switch event {
	case "push":
		befunde = bewertePush(root, s, d, &errs)
	case "delete":
		befunde = bewerteLoeschung(root, &errs)
	default:
		befunde = bewerteRepository(root, &errs)
	}
	if errs.any() {
		return []Befund{ungueltig(payload, event, &errs)}
	}
	return befunde
}

// pathErrors collects the field paths that failed validation, as pydantic's error
// locations do. Only paths ever reach a reason, never values: payloads can carry
// content that must not land in the log.
type pathErrors struct{ paths map[string]struct{} }

func (e *pathErrors) add(parts ...string) {
	if e.paths == nil {
		e.paths = map[string]struct{}{}
	}
	e.paths[strings.Join(parts, ".")] = struct{}{}
}

func (e *pathErrors) any() bool { return len(e.paths) > 0 }

func (e *pathErrors) sorted() []string {
	out := make([]string, 0, len(e.paths))
	for p := range e.paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// decodeObject accepts only a JSON object: null, arrays and scalars are not payloads.
func decodeObject(raw []byte) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

func ungueltig(payload []byte, event string, errs *pathErrors) Befund {
	repo := repoNameLenient(payload)
	if errs == nil || !errs.any() {
		return unpruefbar(repo, nil, fmt.Sprintf(
			"Ereignis %s: Payload ist kein gültiges JSON-Objekt, nicht prüfbar", event))
	}
	return unpruefbar(repo, nil, fmt.Sprintf(
		"Ereignis %s: Pflichtfelder fehlen oder haben falschen Typ (%s), nicht prüfbar",
		event, strings.Join(errs.sorted(), ", ")))
}

// repoNameLenient reads only repository.name so that incomplete payloads still get a name.
func repoNameLenient(payload []byte) string {
	root, ok := decodeObject(payload)
	if !ok {
		return unbekanntesRepo
	}
	var errs pathErrors
	if name, ok := repoName(root, &errs); ok && !errs.any() {
		return name
	}
	return unbekanntesRepo
}

func asString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func asBool(raw json.RawMessage) (value, ok bool) {
	switch string(raw) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// asInt is pydantic's lax int: an integral JSON number or a numeric string.
func asInt(raw json.RawMessage) (int, bool) {
	text := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		s, ok := asString(raw)
		if !ok {
			return 0, false
		}
		text = strings.TrimSpace(s)
	}
	if n, err := strconv.Atoi(text); err == nil {
		return n, true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) || math.Abs(f) > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

func reqString(obj map[string]json.RawMessage, key string, errs *pathErrors) (string, bool) {
	s, ok := asString(obj[key])
	if !ok {
		errs.add(key)
	}
	return s, ok
}

func reqBool(obj map[string]json.RawMessage, key string, errs *pathErrors) (value, ok bool) {
	b, ok := asBool(obj[key])
	if !ok {
		errs.add(key)
	}
	return b, ok
}

// repoName validates repository.name (required, string).
func repoName(root map[string]json.RawMessage, errs *pathErrors) (string, bool) {
	repo, ok := decodeObject(root["repository"])
	if !ok {
		errs.add("repository")
		return "", false
	}
	name, ok := asString(repo["name"])
	if !ok {
		errs.add("repository", "name")
	}
	return name, ok
}

type commitList struct {
	present  bool // false: the field was missing or null
	count    int
	removals int
}

// readCommits validates push.commits: optional array of objects whose optional
// "removed" is an array of strings.
func readCommits(root map[string]json.RawMessage, errs *pathErrors) commitList {
	raw := root["commits"]
	if len(raw) == 0 || string(raw) == "null" {
		return commitList{}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || raw[0] != '[' {
		errs.add("commits")
		return commitList{}
	}
	out := commitList{present: true, count: len(items)}
	for i, item := range items {
		idx := strconv.Itoa(i)
		commit, ok := decodeObject(item)
		if !ok {
			errs.add("commits", idx)
			continue
		}
		removed, has := commit["removed"]
		if !has {
			continue
		}
		var names []json.RawMessage
		if err := json.Unmarshal(removed, &names); err != nil || names == nil {
			errs.add("commits", idx, "removed")
			continue
		}
		for j, n := range names {
			if _, ok := asString(n); !ok {
				errs.add("commits", idx, "removed", strconv.Itoa(j))
				continue
			}
			out.removals++
		}
	}
	return out
}

func bewertePush(root map[string]json.RawMessage, s Schwellen, d *DiffStat, errs *pathErrors) []Befund {
	ref, refOK := reqString(root, "ref", errs)
	forced, _ := reqBool(root, "forced", errs)
	deleted, _ := reqBool(root, "deleted", errs)
	repo, repoOK := repoName(root, errs)
	commits := readCommits(root, errs)
	size, sizeSet := 0, false
	if raw := root["size"]; len(raw) > 0 && string(raw) != "null" {
		n, ok := asInt(raw)
		if !ok {
			errs.add("size")
		}
		size, sizeSet = n, ok
	}
	if errs.any() || !refOK || !repoOK {
		return nil
	}

	var befunde []Befund
	if forced {
		befunde = append(befunde, Befund{ArtForcePush, repo, strRef(ref), SchwereKritisch, "Force-Push auf " + ref})
	}
	if deleted {
		befunde = append(befunde, Befund{ArtRefGeloescht, repo, strRef(ref), SchwereKritisch, ref + " per Push gelöscht"})
	}
	truncated := !commits.present || commits.count >= maxCommitsImPayload || (sizeSet && size > commits.count)
	return append(befunde, massenaenderung(repo, ref, s, d, truncated, commits.removals)...)
}

func massenaenderung(repo, ref string, s Schwellen, d *DiffStat, truncated bool, removals int) []Befund {
	var dateien, zeilen int
	switch {
	case d != nil:
		dateien, zeilen = d.DateienGeloescht, d.ZeilenGeloescht
	case truncated:
		return []Befund{unpruefbar(repo, strRef(ref),
			"Massenänderung nicht prüfbar: Commit-Liste des Pushes unvollständig und keine Diffstat vorhanden")}
	default:
		// Without a diffstat only deleted files can be counted, not lines; the commit list
		// is complete here, so the file count is exact.
		dateien = removals
	}
	if dateien > s.GeloeschteDateien || zeilen > s.GeloeschteZeilen {
		return []Befund{{ArtMassenaenderung, repo, strRef(ref), SchwereWarnung, fmt.Sprintf(
			"%d Dateien und %d Zeilen gelöscht (Schwelle %d Dateien / %d Zeilen)",
			dateien, zeilen, s.GeloeschteDateien, s.GeloeschteZeilen)}}
	}
	return nil
}

func bewerteLoeschung(root map[string]json.RawMessage, errs *pathErrors) []Befund {
	ref, refOK := reqString(root, "ref", errs)
	refType, typeOK := asString(root["ref_type"])
	if !typeOK || (refType != "branch" && refType != "tag") {
		errs.add("ref_type")
	}
	repo, repoOK := repoName(root, errs)
	if errs.any() || !refOK || !repoOK {
		return nil
	}
	// The delete event names only the short ref; findings always carry the full one.
	prefix := "refs/tags/"
	if refType == "branch" {
		prefix = "refs/heads/"
	}
	return []Befund{{ArtRefGeloescht, repo, strRef(prefix + ref), SchwereKritisch,
		refType + " " + ref + " gelöscht"}}
}

// kritischeRepositoryAktion maps the repository actions that destroy or expose
// something. Every other action is either harmless or unknown (-> unpruefbar).
func kritischeRepositoryAktion(action string) (Art, bool) {
	switch action {
	case "deleted":
		return ArtRepoGeloescht, true
	case "transferred":
		return ArtRepoUebertragen, true
	case "renamed":
		return ArtRepoUmbenannt, true
	case "publicized":
		return ArtRepoOeffentlich, true
	default:
		return "", false
	}
}

func harmloseRepositoryAktion(action string) bool {
	switch action {
	case "archived", "unarchived", "privatized", "created", "edited":
		return true
	default:
		return false
	}
}

func bewerteRepository(root map[string]json.RawMessage, errs *pathErrors) []Befund {
	action, actionOK := reqString(root, "action", errs)
	repo, repoOK := repoName(root, errs)
	if errs.any() || !actionOK || !repoOK {
		return nil
	}
	if art, ok := kritischeRepositoryAktion(action); ok {
		return []Befund{{art, repo, nil, SchwereKritisch, "Repository-Ereignis " + action}}
	}
	if harmloseRepositoryAktion(action) {
		return nil
	}
	return []Befund{unpruefbar(repo, nil, "Unbekannte Repository-Aktion "+pyRepr(action)+", nicht prüfbar")}
}

// pyRepr renders s like Python's repr(str), so reasons match the reference byte for
// byte: single quotes unless s holds a single and no double quote, backslash escapes
// for the usual controls, \xNN, \uNNNN and \UNNNNNNNN for unprintable characters.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r == utf8.RuneError && size == 1:
			// Invalid UTF-8 cannot come out of a JSON string; keep the replacement visible.
			b.WriteString(`�`)
		case unicode.IsPrint(r) || r == ' ':
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

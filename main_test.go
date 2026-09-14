//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      options
		wantError bool
	}{
		{"default", nil, options{command: "save"}, false},
		{"ignored", []string{"--include-ignored"}, options{command: "save", includeIgnored: true}, false},
		{"help", []string{"--help"}, options{command: "save", help: true}, false},
		{"list", []string{"LIST"}, options{command: "list"}, false},
		{
			"index",
			[]string{"diff", "A", "B", "--index", "--binary", "--exit-code"},
			options{command: "diff", index: true, names: []string{"A", "B"}, diffOptions: []string{"--binary", "--exit-code"}},
			false,
		},
		{
			"literal paths",
			[]string{"diff", "--", "--help", "odd [x] & space.txt"},
			options{command: "diff", paths: []string{"--help", "odd [x] & space.txt"}},
			false,
		},
		{"delete", []string{"delete", "A"}, options{command: "delete", names: []string{"A"}}, false},
		{"missing delete name", []string{"delete"}, options{}, true},
		{"too many snapshots", []string{"diff", "A", "B", "C"}, options{}, true},
		{"unknown command", []string{"restore", "A"}, options{}, true},
		{"unknown option", []string{"diff", "--no-index"}, options{}, true},
		{"unexpected list option", []string{"list", "--all"}, options{}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseOptions(test.arguments)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %v", err, test.wantError)
			}
			if !test.wantError && !reflect.DeepEqual(got, test.want) {
				t.Fatalf("options = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestInstalledCommandHints(t *testing.T) {
	if !strings.Contains(helpText, "\n  snapshot list\n") {
		t.Fatal("help must advertise the installed snapshot command")
	}
	canonicalHint := func(message string) bool {
		return strings.Contains(message, "'snapshot ") || strings.Contains(message, "'snapshot'")
	}
	for _, arguments := range [][]string{
		{"restore"},
		{"save", "--bad"},
		{"list", "--bad"},
		{"diff", "--bad"},
		{"delete", "--bad"},
		{"delete"},
	} {
		_, err := parseOptions(arguments)
		if err == nil || !canonicalHint(err.Error()) {
			t.Fatalf("command hint for %v: %v", arguments, err)
		}
	}
	for _, name := range []string{"latest", "missing"} {
		_, err := resolveSnapshot(nil, name, headState{branch: "main", label: "main"})
		if err == nil || !canonicalHint(err.Error()) {
			t.Fatalf("snapshot lookup hint: %v", err)
		}
	}
	var output bytes.Buffer
	if err := showList(&output, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Run 'snapshot'") {
		t.Fatalf("empty list hint: %q", output.String())
	}
}

func TestNamesAndSorting(t *testing.T) {
	created := time.Date(2026, 9, 13, 11, 40, 12, 345_000_000, time.FixedZone("", -7*60*60))
	base := "feature-login-2026-09-13-T-11-40"
	if got := newSnapshotName("feature/login", created, nil); got != base {
		t.Fatalf("name = %q, want %q", got, base)
	}
	existing := []snapshot{{metadata: metadata{Name: strings.ToUpper(base)}}}
	for suffix := 2; suffix <= 10; suffix++ {
		existing = append(existing, snapshot{metadata: metadata{Name: base + "-" + strconv.Itoa(suffix)}})
	}
	if got := newSnapshotName("feature/login", created, existing); got != base+"-11" {
		t.Fatalf("collision name = %q", got)
	}
	if got := newSnapshotName("\u4e2d\u6587", created, nil); got != "branch-2026-09-13-T-11-40" {
		t.Fatalf("fallback name = %q", got)
	}
	if got := newSnapshotName(strings.Repeat("a", 100), created, nil); !strings.HasPrefix(got, strings.Repeat("a", 64)+"-2026") {
		t.Fatalf("truncated name = %q", got)
	}
	records := []snapshot{
		{metadata: metadata{Name: "z-2", Branch: "main"}, ticks: 1},
		{metadata: metadata{Name: "a-10", Branch: "other"}, ticks: 3},
		{metadata: metadata{Name: "x-9", Branch: "main"}, ticks: 2},
	}
	sorted := sortedSnapshots(records)
	if sorted[0].Name != "a-10" || sorted[1].Name != "x-9" || records[0].Name != "z-2" {
		t.Fatalf("timestamp order or input preservation failed: %#v", sorted)
	}
	latest, err := resolveSnapshot(records, "latest", headState{branch: "main", label: "main"})
	if err != nil || latest.Name != "x-9" {
		t.Fatalf("branch latest = %#v, error %v", latest, err)
	}
	if _, err := resolveSnapshot(records, "X-9", headState{}); err == nil {
		t.Fatal("explicit snapshot names must remain case-sensitive")
	}
	detached := []snapshot{
		{metadata: metadata{Name: "old", Head: strings.Repeat("1", 40)}, ticks: 1},
		{metadata: metadata{Name: "new", Head: strings.Repeat("2", 40)}, ticks: 2},
	}
	latest, err = resolveSnapshot(detached, "latest", headState{head: strings.Repeat("1", 40)})
	if err != nil || latest.Name != "old" {
		t.Fatalf("detached latest = %#v, error %v", latest, err)
	}
}

func TestTimestampTicks(t *testing.T) {
	tests := []time.Time{
		time.Date(2026, 9, 13, 11, 40, 12, 345_678_900, time.FixedZone("", -7*60*60)),
		time.Date(1960, 1, 1, 12, 30, 1, 100, time.FixedZone("", 330*60)),
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999_999_900, time.UTC),
	}
	for _, original := range tests {
		_, seconds := original.Zone()
		restored, err := timeFromTicks(utcTicks(original), seconds/60)
		if err != nil || !restored.Equal(original) || restored.Format(time.RFC3339Nano) != original.Format(time.RFC3339Nano) {
			t.Fatalf("round trip %s => %s, error %v", original, restored, err)
		}
	}
	if utcTicks(time.Unix(0, 0)) != unixEpochTicks {
		t.Fatal("unexpected Unix epoch tick offset")
	}
	for _, item := range [][2]int64{{-1, 0}, {maxTimestampTicks + 1, 0}, {unixEpochTicks, 841}, {0, -1}} {
		if _, err := timeFromTicks(item[0], int(item[1])); err == nil {
			t.Fatalf("accepted invalid timestamp/offset %v", item)
		}
	}
}

func validRecord(t *testing.T, oidLength int) (repository, []string, metadata) {
	t.Helper()
	repo := repository{worktreeID: "main", refPrefix: "refs/snapshots/v1/main/"}
	record := metadata{
		Version: 1, Name: "main-2026-09-13-T-11-40", Branch: "main", Head: strings.Repeat("1", oidLength),
		CreatedUTCTicks:  strconv.FormatInt(utcTicks(time.Date(2026, 9, 13, 18, 40, 0, 0, time.UTC)), 10),
		UTCOffsetMinutes: -420, WorktreeID: "main", IncludeIgnored: false, IndexCommit: strings.Repeat("2", oidLength),
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	fields := []string{repo.refPrefix + record.Name, strings.Repeat("3", oidLength), record.IndexCommit, metadataPrefix + string(payload)}
	return repo, fields, record
}

func TestSnapshotMetadata(t *testing.T) {
	for _, length := range []int{40, 64} {
		repo, fields, expected := validRecord(t, length)
		actual, err := parseSnapshotRecord(repo, fields)
		if err != nil || actual.metadata != expected {
			t.Fatalf("metadata round trip: %#v, error %v", actual, err)
		}
		if actual.created.Format("2006-01-02-T-15-04") != "2026-09-13-T-11-40" {
			t.Fatal("metadata offset was not retained")
		}
	}
	tests := map[string]func([]string){
		"foreign namespace": func(fields []string) { fields[0] = "refs/heads/main" },
		"wrong parent":      func(fields []string) { fields[2] = strings.Repeat("4", 40) },
		"bad version":       func(fields []string) { fields[3] = strings.Replace(fields[3], `"version":1`, `"version":2`, 1) },
		"missing boolean":   func(fields []string) { fields[3] = strings.Replace(fields[3], `"includeIgnored":false,`, "", 1) },
		"null branch":       func(fields []string) { fields[3] = strings.Replace(fields[3], `"branch":"main"`, `"branch":null`, 1) },
		"invalid JSON":      func(fields []string) { fields[3] = metadataPrefix + "{" },
		"bad commit ID":     func(fields []string) { fields[1] = "not-an-object" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			repo, fields, _ := validRecord(t, 40)
			change(fields)
			if _, err := parseSnapshotRecord(repo, fields); err == nil {
				t.Fatal("invalid record was accepted")
			}
		})
	}
}

func TestByteAndEnvironmentHandling(t *testing.T) {
	environment := mergedEnvironment([]string{"Path=original", "git_index_file=old", "OTHER=retained"},
		map[string]string{"PATH": "replacement", "GIT_INDEX_FILE": "private"})
	if strings.Join(environment, "|") != "OTHER=retained|GIT_INDEX_FILE=private|PATH=replacement" {
		t.Fatalf("case-insensitive environment replacement failed: %v", environment)
	}
	path := "odd [x] & \u00e9\u4e2d.txt"
	entries := []byte("100644 " + strings.Repeat("1", 40) + " 0\t" + path + "\x00")
	if err := assertNoGitlinks(entries); err != nil {
		t.Fatal(err)
	}
	link := []byte("160000 " + strings.Repeat("1", 40) + " 0\tnested\x00")
	if err := assertNoGitlinks(link); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("gitlink error = %v", err)
	}
	if _, err := gitText([]byte{0xff}); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
	text, err := gitText([]byte(" preserve spaces \r\n"))
	if err != nil || text != " preserve spaces " {
		t.Fatalf("Git text = %q, error %v", text, err)
	}
}

func TestLockAndIndexIsolation(t *testing.T) {
	directory := t.TempDir()
	repo := repository{gitDirectory: directory}
	lock, err := acquireSnapshotLock(repo)
	if err != nil {
		t.Fatal(err)
	}
	second, secondError := acquireSnapshotLock(repo)
	if secondError == nil {
		if err := second.Close(); err != nil {
			t.Errorf("close second lock: %v", err)
		}
		if err := lock.Close(); err != nil {
			t.Errorf("close first lock: %v", err)
		}
		t.Fatal("concurrent lock was accepted")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := withSnapshotLock(repo, func() error { return errors.New("expected failure") }); err == nil {
		t.Fatal("operation error was lost")
	}
	if err := withSnapshotLock(repo, func() error { return nil }); err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	path := filepath.Join(directory, "index")
	missing, err := readSourceIndex(path)
	if err != nil || missing.exists {
		t.Fatalf("missing index: %#v, %v", missing, err)
	}
	data := []byte{0, 1, 2, 255}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := readSourceIndex(path)
	if err != nil || !index.exists || !bytes.Equal(index.data, data) {
		t.Fatalf("source index: %#v, %v", index, err)
	}
	if _, err := readSourceIndex(directory); err == nil {
		t.Fatal("index read errors must not become an empty staging area")
	}
}

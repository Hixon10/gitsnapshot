package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
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
		{"list alias", []string{"ls"}, options{command: "list"}, false},
		{"uppercase list alias", []string{"LS"}, options{command: "list"}, false},
		{"list alias help", []string{"ls", "--help"}, options{command: "list", help: true}, false},
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
		{"unexpected list alias option", []string{"ls", "--all"}, options{}, true},
		{"unexpected list alias argument", []string{"ls", "extra"}, options{}, true},
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
	if !strings.Contains(helpText, `"ls" is an alias for "list".`) {
		t.Fatal("help must advertise the list alias")
	}
	canonicalHint := func(message string) bool {
		return strings.Contains(message, "'snapshot ") || strings.Contains(message, "'snapshot'")
	}
	for _, arguments := range [][]string{
		{"restore"},
		{"save", "--bad"},
		{"list", "--bad"},
		{"ls", "--bad"},
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

func TestEnvironmentHandling(t *testing.T) {
	environment := mergedEnvironment([]string{
		"Path=original", "PATH=old", "git_index_file=old", "GIT_INDEX_FILE=old", "OTHER=retained",
	},
		map[string]string{"PATH": "replacement", "GIT_INDEX_FILE": "private"})
	want := "OTHER=retained|GIT_INDEX_FILE=private|PATH=replacement"
	if runtime.GOOS != "windows" {
		want = "Path=original|git_index_file=old|" + want
	}
	if strings.Join(environment, "|") != want {
		t.Fatalf("environment = %v, want %s", environment, want)
	}
}

func TestByteHandling(t *testing.T) {
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

func TestDirectoryIdentity(t *testing.T) {
	directory := t.TempDir()
	alias := directory + string(os.PathSeparator) + "."
	if same, err := sameDirectory(directory, alias); err != nil || !same {
		t.Fatalf("directory alias = %v, error %v", same, err)
	}
	lower := filepath.Join(directory, "case")
	upper := filepath.Join(directory, "CASE")
	if err := os.Mkdir(lower, 0o700); err != nil {
		t.Fatal(err)
	}
	err := os.Mkdir(upper, 0o700)
	wantSame := errors.Is(err, os.ErrExist)
	if err != nil && !wantSame {
		t.Fatal(err)
	}
	if same, err := sameDirectory(lower, upper); err != nil || same != wantSame {
		t.Fatalf("case-sensitive filesystem identity = %v, want %v, error %v", same, wantSame, err)
	}
	if same, err := sameDirectory(directory, lower); err != nil || same {
		t.Fatalf("distinct directories = %v, error %v", same, err)
	}
	if _, err := sameDirectory(directory, filepath.Join(directory, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory error = %v", err)
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
	badRepo := repository{gitDirectory: filepath.Join(directory, "missing")}
	called := false
	if err := withSnapshotLock(badRepo, func() error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatalf("failed acquisition allowed an operation: called=%v, error=%v", called, err)
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

func isolateGitEnvironment(t *testing.T, directory string) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	config := filepath.Join(directory, "empty.config")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	templates := filepath.Join(directory, "templates")
	if err := os.Mkdir(templates, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_TEMPLATE_DIR", templates)
}

func TestGitSnapshotRoundTrip(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			directory := t.TempDir()
			isolateGitEnvironment(t, directory)
			root := filepath.Join(directory, "work tree [x] & space")
			if runtime.GOOS != "windows" {
				root += "\r"
			}
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			if runtime.GOOS == "windows" {
				// Git objects are read-only; clear that attribute before TempDir cleanup.
				t.Cleanup(func() {
					err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if entry.Type().IsRegular() {
							return os.Chmod(path, 0o600)
						}
						return nil
					})
					if err != nil {
						t.Errorf("prepare fixture cleanup: %v", err)
					}
				})
			}
			gitExecutable, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			var diagnostics bytes.Buffer
			app := application{gitExecutable: gitExecutable, output: io.Discard, errorOutput: &diagnostics}
			git := func(arguments ...string) string {
				t.Helper()
				result, err := app.git(root, arguments, nil, nil)
				if err != nil {
					t.Fatalf("git %v: %v", arguments, err)
				}
				return string(result.output)
			}
			write := func(name, contents string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cli := func(wantCode int, arguments ...string) string {
				t.Helper()
				var output, errorOutput bytes.Buffer
				code, err := runCLI(arguments, &output, &errorOutput)
				if err != nil || code != wantCode {
					t.Fatalf("snapshot %v: code=%d, want=%d, error=%v, stderr=%s",
						arguments, code, wantCode, err, errorOutput.String())
				}
				return output.String()
			}
			git("init", "--quiet", "--initial-branch=main", "--object-format="+objectFormat)
			git("config", "core.autocrlf", "false")
			git("config", "user.name", "Snapshot Tests")
			git("config", "user.email", "tests@example.invalid")
			write(".gitignore", "*.ignored\n")
			write("tracked.txt", "base\n")
			if runtime.GOOS != "windows" {
				write("executable", "content\n")
			}
			git("add", "--", ".")
			git("commit", "--quiet", "-m", "Initial fixture")
			write("tracked.txt", "staged\n")
			git("add", "--", "tracked.txt")
			write("tracked.txt", "working\n")
			write("local.config", "before\n")
			write("cache.ignored", "ignored\n")
			if runtime.GOOS != "windows" {
				if err := os.Symlink("tracked.txt", filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
			}
			repo, err := app.repository(root)
			if err != nil || repo.worktreeID != "main" {
				t.Fatalf("main worktree discovery: %#v, error %v", repo, err)
			}
			headBefore := git("rev-parse", "HEAD")
			indexBefore, err := os.ReadFile(repo.indexPath)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := cli(0, "ls"), cli(0, "list"); got != want {
				t.Fatalf("empty list alias output = %q, want %q", got, want)
			}
			first := strings.TrimSpace(cli(0, "save"))
			ref := repo.refPrefix + first
			if !strings.HasPrefix(first, "main-") {
				t.Fatalf("snapshot name = %q", first)
			}
			if got := git("show", ref+":tracked.txt"); got != "working\n" {
				t.Fatalf("working tree contents = %q", got)
			}
			if got := git("show", ref+"^:tracked.txt"); got != "staged\n" {
				t.Fatalf("staged tree contents = %q", got)
			}
			if got := git("show", ref+":local.config"); got != "before\n" {
				t.Fatalf("untracked contents = %q", got)
			}
			if got := git("ls-tree", "-r", "--name-only", ref); strings.Contains(got, "cache.ignored") {
				t.Fatalf("ignored file was captured: %s", got)
			}
			if runtime.GOOS != "windows" {
				if got := git("ls-tree", ref, "--", "link"); !strings.HasPrefix(got, "120000 ") {
					t.Fatalf("symlink mode = %q", got)
				}
				if got := git("show", ref+":link"); got != "tracked.txt" {
					t.Fatalf("symlink target = %q", got)
				}
				if got := git("ls-tree", ref, "--", "executable"); !strings.HasPrefix(got, "100644 ") {
					t.Fatalf("original executable mode = %q", got)
				}
			}
			write("tracked.txt", "agent changes\n")
			write("local.config", "after\n")
			write("new [x] & space.txt", "new\n")
			wantPaths := []string{"local.config", "new [x] & space.txt", "tracked.txt"}
			if runtime.GOOS != "windows" {
				if err := os.Chmod(filepath.Join(root, "executable"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("local.config", filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				wantPaths = append(wantPaths, "executable", "link")
			}
			sort.Strings(wantPaths)
			wantDiff := strings.Join(wantPaths, "\n") + "\n"
			if got := cli(1, "diff", first, "--name-only", "--exit-code"); got != wantDiff {
				t.Fatalf("current diff = %q, want %q", got, wantDiff)
			}
			if got := cli(1, "diff", first, "--name-only", "--exit-code", "--", "new [x] & space.txt"); got != "new [x] & space.txt\n" {
				t.Fatalf("literal path diff = %q", got)
			}
			if got := cli(0, "diff", first, "--index", "--name-only", "--exit-code"); got != "" {
				t.Fatalf("unchanged staged diff = %q", got)
			}
			second := strings.TrimSpace(cli(0, "save"))
			if first == second {
				t.Fatal("save reused a snapshot name")
			}
			if runtime.GOOS != "windows" {
				secondRef := repo.refPrefix + second
				if got := git("ls-tree", secondRef, "--", "executable"); !strings.HasPrefix(got, "100755 ") {
					t.Fatalf("changed executable mode = %q", got)
				}
				if got := git("show", secondRef+":link"); got != "local.config" {
					t.Fatalf("changed symlink target = %q", got)
				}
			}
			if got := cli(1, "diff", first, second, "--name-only", "--exit-code"); got != wantDiff {
				t.Fatalf("saved diff = %q, want %q", got, wantDiff)
			}
			if got := cli(0, "diff", "--name-only", "--exit-code"); got != "" {
				t.Fatalf("latest snapshot differs immediately after save: %q", got)
			}
			listOutput := cli(0, "list")
			for _, alias := range []string{"ls", "LS"} {
				if got := cli(0, alias); got != listOutput {
					t.Fatalf("%s output = %q, want %q", alias, got, listOutput)
				}
			}
			lines := strings.Split(strings.TrimSpace(listOutput), "\n")
			if len(lines) != 3 || strings.Fields(lines[1])[0] != second || strings.Fields(lines[2])[0] != first {
				t.Fatalf("snapshot ordering = %v", lines)
			}
			linkedRoot := filepath.Join(directory, "linked worktree")
			git("worktree", "add", "--quiet", "-b", "linked", linkedRoot, "HEAD")
			linkedRepo, err := app.repository(linkedRoot)
			if err != nil || !strings.HasPrefix(linkedRepo.worktreeID, "linked-") {
				t.Fatalf("linked worktree discovery: %#v, error %v", linkedRepo, err)
			}
			if _, err := app.save(linkedRepo, false); err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				repo  repository
				count int
			}{{repo, 2}, {linkedRepo, 1}} {
				snapshots, err := app.snapshots(item.repo)
				if err != nil || len(snapshots) != item.count {
					t.Fatalf("worktree snapshot isolation: got=%d, want=%d, error=%v", len(snapshots), item.count, err)
				}
			}
			cli(0, "delete", first)
			snapshots, err := app.snapshots(repo)
			if err != nil || len(snapshots) != 1 || snapshots[0].Name != second {
				t.Fatalf("snapshot deletion: %v, error %v", snapshots, err)
			}
			if got := git("rev-parse", "HEAD"); got != headBefore {
				t.Fatalf("HEAD changed: %q, want %q", got, headBefore)
			}
			indexAfter, err := os.ReadFile(repo.indexPath)
			if err != nil || !bytes.Equal(indexBefore, indexAfter) {
				t.Fatalf("real index changed, read error %v", err)
			}
			for name, want := range map[string]string{
				"tracked.txt": "agent changes\n", "local.config": "after\n",
				"new [x] & space.txt": "new\n", "cache.ignored": "ignored\n",
			} {
				got, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || string(got) != want {
					t.Fatalf("working file %s changed: %q, error %v", name, got, err)
				}
			}
			entries, err := os.ReadDir(repo.gitDirectory)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "gitsnapshot-") && strings.Contains(entry.Name(), ".index") {
					t.Fatalf("temporary index was not cleaned up: %s", entry.Name())
				}
			}
		})
	}
}

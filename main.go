//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	metadataPrefix    = "git-snapshot-v1 "
	ticksPerSecond    = int64(10_000_000)
	unixEpochTicks    = int64(621355968000000000)
	maxTimestampTicks = int64(3155378975999999999)
)

const helpText = `Usage:
  snapshot [save] [--include-ignored]
  snapshot list
  snapshot diff [<snapshot> [<snapshot>]] [options] [-- <paths>...]
  snapshot delete <snapshot>

Snapshot names use local time: <branch>-2026-09-13-T-11-40.
Same-minute names receive -2, -3, etc. List is newest first.

Diff defaults to latest -> current files, including untracked files.
One name compares that snapshot with current files; two compare snapshots.
"latest" selects the newest snapshot for this branch in this worktree.

Diff options:
  --index        Compare staging areas instead of working files.
  --stat         Show a change summary.
  --name-only    Show changed paths only.
  --name-status  Show changed paths and change types.
  --binary       Include binary patches.
  --exit-code    Exit 1 for differences, 0 for no differences.

All captures cover the repository, even when run from a subdirectory.
Diff path filters are literal paths relative to the current directory.
Ignored files are excluded unless saved with --include-ignored.
The working files, real index, HEAD, and normal stash are not changed.
Restore, submodule contents, and sparse checkouts are not supported.
Errors exit 2. No Git fetch, push, checkout, or normal commit is performed.
`

var (
	objectIDPattern    = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	branchLabelPattern = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	gitOptions         = []string{
		"--no-pager", "--literal-pathspecs",
		"-c", "core.splitIndex=false",
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "core.ignoreStat=false",
		"-c", "core.quotePath=false",
	}
)

type options struct {
	command        string
	help           bool
	includeIgnored bool
	index          bool
	names          []string
	paths          []string
	diffOptions    []string
}

type repository struct {
	root                string
	gitDirectory        string
	indexPath           string
	invocationDirectory string
	worktreeID          string
	refPrefix           string
}

type headState struct {
	branch    string
	branchRef string
	head      string
	label     string
}

type sourceIndex struct {
	exists bool
	data   []byte
	hash   [sha256.Size]byte
}

type capturedState struct {
	head        headState
	sourceIndex sourceIndex
	indexTree   string
	workingTree string
}

type metadata struct {
	Version          int    `json:"version"`
	Name             string `json:"name"`
	Branch           string `json:"branch"`
	Head             string `json:"head"`
	CreatedUTCTicks  string `json:"createdUtcTicks"`
	UTCOffsetMinutes int    `json:"utcOffsetMinutes"`
	WorktreeID       string `json:"worktreeId"`
	IncludeIgnored   bool   `json:"includeIgnored"`
	IndexCommit      string `json:"indexCommit"`
}

type snapshot struct {
	metadata
	ref     string
	oid     string
	ticks   int64
	created time.Time
}

type gitResult struct {
	output   []byte
	exitCode int
}

type application struct {
	gitExecutable string
	output        io.Writer
	errorOutput   io.Writer
}

func main() {
	code, err := runCLI(os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "snapshot:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func parseOptions(arguments []string) (options, error) {
	result := options{command: "save"}
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		result.command = strings.ToLower(arguments[0])
		arguments = arguments[1:]
	}
	switch result.command {
	case "help":
		result.help = true
		return result, nil
	case "save", "list", "diff", "delete":
	default:
		return result, fmt.Errorf("Unknown command %q. Run 'snapshot --help'.", result.command)
	}
	paths := false
	for _, argument := range arguments {
		if paths {
			result.paths = append(result.paths, argument)
			continue
		}
		option := strings.ToLower(argument)
		if option == "--help" || option == "-h" {
			result.help = true
			continue
		}
		switch result.command {
		case "save":
			if option != "--include-ignored" {
				return result, fmt.Errorf("Unknown save option %q. Run 'snapshot --help'.", argument)
			}
			result.includeIgnored = true
		case "list":
			return result, fmt.Errorf("Unexpected list argument %q. Run 'snapshot --help'.", argument)
		case "delete":
			if strings.HasPrefix(argument, "-") {
				return result, fmt.Errorf("Unknown delete option %q. Run 'snapshot --help'.", argument)
			}
			result.names = append(result.names, argument)
		case "diff":
			switch option {
			case "--":
				paths = true
			case "--index":
				result.index = true
			case "--stat", "--name-only", "--name-status", "--binary", "--exit-code":
				result.diffOptions = append(result.diffOptions, option)
			default:
				if strings.HasPrefix(argument, "-") {
					return result, fmt.Errorf("Unknown diff option %q. Run 'snapshot --help'.", argument)
				}
				result.names = append(result.names, argument)
			}
		}
	}
	if !result.help {
		if result.command == "delete" && len(result.names) != 1 {
			return result, errors.New("Use 'snapshot delete <snapshot>'.")
		}
		if result.command == "diff" && len(result.names) > 2 {
			return result, errors.New("Diff accepts at most two snapshot names. Put file paths after --.")
		}
	}
	return result, nil
}

func runCLI(arguments []string, output, errorOutput io.Writer) (int, error) {
	opts, err := parseOptions(arguments)
	if err != nil {
		return 0, err
	}
	if opts.help {
		_, err = io.WriteString(output, helpText)
		return 0, err
	}
	directory, err := os.Getwd()
	if err != nil {
		return 0, fmt.Errorf("get working directory: %w", err)
	}
	gitExecutable, err := exec.LookPath("git.exe")
	if err != nil {
		return 0, fmt.Errorf("find Git for Windows on PATH: %w", err)
	}
	app := application{gitExecutable: gitExecutable, output: output, errorOutput: errorOutput}
	repo, err := app.repository(directory)
	if err != nil {
		return 0, err
	}
	if opts.command == "save" {
		name, err := app.save(repo, opts.includeIgnored)
		if err != nil {
			return 0, err
		}
		_, err = fmt.Fprintln(output, name)
		return 0, err
	}
	snapshots, err := app.snapshots(repo)
	if err != nil {
		return 0, err
	}
	if opts.command == "list" {
		return 0, showList(output, snapshots)
	}
	head, err := app.headState(repo)
	if err != nil {
		return 0, err
	}
	if opts.command == "delete" {
		selected, err := resolveSnapshot(snapshots, opts.names[0], head)
		if err != nil {
			return 0, err
		}
		err = withSnapshotLock(repo, func() error {
			_, err := app.git(repo.root, []string{"update-ref", "-d", selected.ref, selected.oid}, nil, nil)
			return err
		})
		if err != nil {
			return 0, err
		}
		_, err = fmt.Fprintln(output, "Deleted "+selected.Name)
		return 0, err
	}
	return app.compare(repo, opts, snapshots, head)
}

func mergedEnvironment(base []string, overrides map[string]string) []string {
	overridden := make(map[string]bool, len(overrides))
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		overridden[strings.ToUpper(key)] = true
		keys = append(keys, key)
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if !overridden[strings.ToUpper(key)] {
			result = append(result, entry)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func (app *application) git(directory string, arguments []string, environment map[string]string, input []byte, allowedCodes ...int) (gitResult, error) {
	overrides := map[string]string{
		"GIT_OPTIONAL_LOCKS":  "0",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_NO_LAZY_FETCH":   "1",
	}
	for key, value := range environment {
		overrides[key] = value
	}
	commandArguments := append(append([]string(nil), gitOptions...), arguments...)
	command := exec.Command(app.gitExecutable, commandArguments...)
	command.Dir = directory
	command.Env = mergedEnvironment(os.Environ(), overrides)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var output, errorOutput bytes.Buffer
	command.Stdout = &output
	command.Stderr = &errorOutput
	err := command.Run()
	code := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return gitResult{}, fmt.Errorf("start or communicate with git: %w", err)
		}
		code = exitError.ExitCode()
	}
	allowed := code == 0 && len(allowedCodes) == 0
	for _, expected := range allowedCodes {
		allowed = allowed || code == expected
	}
	if !allowed {
		return gitResult{}, fmt.Errorf("git %s failed (exit %d): %s", arguments[0], code, strings.TrimSpace(errorOutput.String()))
	}
	if errorOutput.Len() > 0 {
		if _, err := app.errorOutput.Write(errorOutput.Bytes()); err != nil {
			return gitResult{}, fmt.Errorf("write Git stderr: %w", err)
		}
	}
	return gitResult{output: output.Bytes(), exitCode: code}, nil
}

func gitText(data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", errors.New("Git returned invalid UTF-8 text.")
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func (app *application) text(directory string, arguments []string, environment map[string]string, input []byte) (string, error) {
	result, err := app.git(directory, arguments, environment, input)
	if err != nil {
		return "", err
	}
	return gitText(result.output)
}

func (app *application) repository(directory string) (repository, error) {
	if os.Getenv("GIT_INDEX_FILE") != "" {
		return repository{}, errors.New("Unset GIT_INDEX_FILE before using snapshot; an overridden source index is not supported.")
	}
	paths, err := app.text(directory, []string{
		"rev-parse", "--path-format=absolute", "--show-toplevel",
		"--absolute-git-dir", "--git-common-dir", "--git-path", "index",
	}, nil, nil)
	if err != nil {
		return repository{}, err
	}
	parts := strings.Split(paths, "\n")
	if len(parts) != 4 {
		return repository{}, errors.New("Git repository discovery did not return four paths.")
	}
	for index, path := range parts {
		path = strings.TrimSuffix(path, "\r")
		if path == "" {
			return repository{}, errors.New("Git repository discovery returned an empty path.")
		}
		parts[index], err = filepath.Abs(path)
		if err != nil {
			return repository{}, fmt.Errorf("resolve Git path: %w", err)
		}
	}
	worktreeID := "main"
	if !strings.EqualFold(parts[1], parts[2]) {
		hash := sha256.Sum256([]byte(filepath.Base(parts[1])))
		worktreeID = "linked-" + hex.EncodeToString(hash[:])
	}
	return repository{
		root: parts[0], gitDirectory: parts[1], indexPath: parts[3],
		invocationDirectory: directory, worktreeID: worktreeID,
		refPrefix: "refs/snapshots/v1/" + worktreeID + "/",
	}, nil
}

func (app *application) headState(repo repository) (headState, error) {
	branchResult, err := app.git(repo.root, []string{"symbolic-ref", "--quiet", "HEAD"}, nil, nil, 0, 1)
	if err != nil {
		return headState{}, err
	}
	branchRef, err := gitText(branchResult.output)
	if err != nil {
		return headState{}, err
	}
	if branchRef != "" && !strings.HasPrefix(branchRef, "refs/heads/") {
		return headState{}, fmt.Errorf("Unsupported HEAD reference: %s", branchRef)
	}
	headResult, err := app.git(repo.root, []string{"rev-parse", "--verify", "--quiet", "HEAD^{commit}"}, nil, nil, 0, 1)
	if err != nil {
		return headState{}, err
	}
	head, err := gitText(headResult.output)
	if err != nil {
		return headState{}, err
	}
	branch := strings.TrimPrefix(branchRef, "refs/heads/")
	if (branch == "" && head == "") || (head != "" && !objectIDPattern.MatchString(head)) {
		return headState{}, errors.New("HEAD is neither a branch nor a valid detached commit.")
	}
	label := branch
	if label == "" {
		label = "detached-" + head[:12]
	}
	return headState{branch: branch, branchRef: branchRef, head: head, label: label}, nil
}

func readSourceIndex(path string) (sourceIndex, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sourceIndex{}, nil
	}
	if err != nil {
		return sourceIndex{}, fmt.Errorf("read source index: %w", err)
	}
	return sourceIndex{exists: true, data: data, hash: sha256.Sum256(data)}, nil
}

func (app *application) assertSourceUnchanged(repo repository, original capturedState) error {
	head, err := app.headState(repo)
	if err != nil {
		return err
	}
	index, err := readSourceIndex(repo.indexPath)
	if err != nil {
		return err
	}
	if head.head != original.head.head || head.branchRef != original.head.branchRef ||
		index.exists != original.sourceIndex.exists || index.hash != original.sourceIndex.hash {
		return errors.New("HEAD or the real index changed during capture. Stop concurrent Git operations and retry.")
	}
	return nil
}

func acquireSnapshotLock(repo repository) (*os.File, error) {
	path := filepath.Join(repo.gitDirectory, "gitsnapshot.lock")
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot lock path: %w", err)
	}
	// Disallow sharing while this process owns the lock handle.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("Cannot acquire the snapshot lock; another snapshot command may be running: %w", err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return nil, errors.Join(errors.New("Cannot open the snapshot lock handle."), syscall.CloseHandle(handle))
	}
	return file, nil
}

func withSnapshotLock(repo repository, operation func() error) (err error) {
	lock, err := acquireSnapshotLock(repo)
	if err != nil {
		return err
	}
	defer func() {
		if closeError := lock.Close(); closeError != nil {
			err = errors.Join(err, fmt.Errorf("close snapshot lock: %w", closeError))
		}
	}()
	return operation()
}

func assertNoGitlinks(entries []byte) error {
	if !utf8.Valid(entries) {
		return errors.New("Git index entries contain invalid UTF-8 paths.")
	}
	for _, entry := range bytes.Split(entries, []byte{0}) {
		if bytes.HasPrefix(entry, []byte("160000 ")) {
			_, path, found := bytes.Cut(entry, []byte{'\t'})
			if !found {
				return errors.New("Git returned a malformed gitlink index entry.")
			}
			return fmt.Errorf("Submodule or nested repository '%s' cannot be captured completely. Snapshot it separately; no snapshot was saved.", path)
		}
	}
	return nil
}

func removeTemporaryIndex(path string) error {
	var result error
	for _, candidate := range []string{path, path + ".lock"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove temporary index file: %w", err))
		}
	}
	return result
}

func (app *application) capture(repo repository, workingFiles, includeIgnored bool, baseline string) (captured capturedState, err error) {
	if workingFiles {
		result, err := app.git(repo.root, []string{"config", "--bool", "--get", "core.sparseCheckout"}, nil, nil, 0, 1)
		if err != nil {
			return captured, err
		}
		if strings.TrimSpace(string(result.output)) == "true" {
			return captured, errors.New("Working-file snapshots do not support sparse checkouts.")
		}
	}
	captured.head, err = app.headState(repo)
	if err != nil {
		return captured, err
	}
	captured.sourceIndex, err = readSourceIndex(repo.indexPath)
	if err != nil {
		return captured, err
	}
	temporary, err := os.CreateTemp(repo.gitDirectory, "gitsnapshot-*.index")
	if err != nil {
		return captured, fmt.Errorf("create temporary index: %w", err)
	}
	path := temporary.Name()
	defer func() { err = errors.Join(err, removeTemporaryIndex(path)) }()
	if captured.sourceIndex.exists {
		_, writeError := temporary.Write(captured.sourceIndex.data)
		err = errors.Join(writeError, temporary.Close())
	} else {
		err = temporary.Close()
		if err == nil {
			err = os.Remove(path)
		}
	}
	if err != nil {
		return captured, fmt.Errorf("prepare temporary index: %w", err)
	}
	environment := map[string]string{"GIT_INDEX_FILE": path}
	indexGit := func(arguments []string, input []byte) (gitResult, error) {
		return app.git(repo.root, arguments, environment, input)
	}
	if captured.sourceIndex.exists {
		_, err = indexGit([]string{"update-index", "--no-split-index"}, nil)
	} else {
		_, err = indexGit([]string{"read-tree", "--empty"}, nil)
	}
	if err != nil {
		return captured, err
	}
	unmerged, err := indexGit([]string{"ls-files", "--unmerged", "-z"}, nil)
	if err != nil {
		return captured, err
	}
	if len(unmerged.output) != 0 {
		return captured, errors.New("Cannot capture an unresolved merge. Resolve the index conflicts first.")
	}
	captured.indexTree, err = app.text(repo.root, []string{"write-tree"}, environment, nil)
	if err != nil {
		return captured, err
	}
	if workingFiles {
		entries, err := indexGit([]string{"ls-files", "--stage", "-z"}, nil)
		if err != nil {
			return captured, err
		}
		if err := assertNoGitlinks(entries.output); err != nil {
			return captured, err
		}
		if baseline != "" {
			// Revisit captured paths even after ignore rules change, then overlay new tracked paths.
			if _, err := indexGit([]string{"read-tree", baseline}, nil); err != nil {
				return captured, err
			}
			if _, err := indexGit([]string{"update-index", "--add", "--replace", "-z", "--index-info"}, entries.output); err != nil {
				return captured, err
			}
		}
		paths, err := indexGit([]string{"ls-files", "-z"}, nil)
		if err != nil {
			return captured, err
		}
		for _, flag := range []string{"--no-assume-unchanged", "--no-skip-worktree"} {
			if _, err := indexGit([]string{"update-index", flag, "-z", "--stdin"}, paths.output); err != nil {
				return captured, err
			}
		}
		arguments := []string{"add", "--all"}
		if includeIgnored {
			arguments = append(arguments, "--force")
		}
		if _, err := indexGit(append(arguments, "--", "."), nil); err != nil {
			return captured, err
		}
		updated, err := indexGit([]string{"ls-files", "--stage", "-z"}, nil)
		if err != nil {
			return captured, err
		}
		if err := assertNoGitlinks(updated.output); err != nil {
			return captured, err
		}
		captured.workingTree, err = app.text(repo.root, []string{"write-tree"}, environment, nil)
		if err != nil {
			return captured, err
		}
	}
	return captured, app.assertSourceUnchanged(repo, captured)
}

func utcTicks(created time.Time) int64 {
	return created.Unix()*ticksPerSecond + int64(created.Nanosecond())/100 + unixEpochTicks
}

func timeFromTicks(ticks int64, offsetMinutes int) (time.Time, error) {
	if ticks < 0 || ticks > maxTimestampTicks || offsetMinutes < -14*60 || offsetMinutes > 14*60 {
		return time.Time{}, errors.New("Snapshot timestamp or UTC offset is out of range.")
	}
	delta := ticks - unixEpochTicks
	created := time.Unix(delta/ticksPerSecond, delta%ticksPerSecond*100).
		In(time.FixedZone("", offsetMinutes*60))
	if created.Year() < 1 || created.Year() > 9999 {
		return time.Time{}, errors.New("Snapshot local timestamp is out of range.")
	}
	return created, nil
}

func parseSnapshotRecord(repo repository, fields []string) (snapshot, error) {
	if len(fields) != 4 || !strings.HasPrefix(fields[0], repo.refPrefix) ||
		!strings.HasPrefix(fields[3], metadataPrefix) {
		return snapshot{}, fmt.Errorf("Invalid snapshot record under %s.", repo.refPrefix)
	}
	payload := []byte(strings.TrimPrefix(fields[3], metadataPrefix))
	var required map[string]json.RawMessage
	if err := json.Unmarshal(payload, &required); err != nil {
		return snapshot{}, fmt.Errorf("Invalid snapshot '%s': %w", fields[0], err)
	}
	for _, key := range []string{
		"version", "name", "branch", "head", "createdUtcTicks", "utcOffsetMinutes",
		"worktreeId", "includeIgnored", "indexCommit",
	} {
		value, exists := required[key]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return snapshot{}, fmt.Errorf("Invalid snapshot '%s': missing %s.", fields[0], key)
		}
	}
	var record metadata
	if err := json.Unmarshal(payload, &record); err != nil {
		return snapshot{}, fmt.Errorf("Invalid snapshot '%s': %w", fields[0], err)
	}
	if record.Version != 1 || record.Name != strings.TrimPrefix(fields[0], repo.refPrefix) ||
		record.Name == "" || record.WorktreeID != repo.worktreeID ||
		!objectIDPattern.MatchString(fields[1]) || !objectIDPattern.MatchString(record.IndexCommit) ||
		fields[2] != record.IndexCommit ||
		(record.Head != "" && !objectIDPattern.MatchString(record.Head)) ||
		(record.Branch == "" && record.Head == "") {
		return snapshot{}, fmt.Errorf("Invalid snapshot '%s': metadata does not match its reference or parent.", fields[0])
	}
	ticks, err := strconv.ParseInt(record.CreatedUTCTicks, 10, 64)
	if err != nil {
		return snapshot{}, fmt.Errorf("Invalid snapshot '%s': %w", fields[0], err)
	}
	created, err := timeFromTicks(ticks, record.UTCOffsetMinutes)
	if err != nil {
		return snapshot{}, fmt.Errorf("Invalid snapshot '%s': %w", fields[0], err)
	}
	return snapshot{metadata: record, ref: fields[0], oid: fields[1], ticks: ticks, created: created}, nil
}

func (app *application) snapshots(repo repository) ([]snapshot, error) {
	text, err := app.text(repo.root, []string{
		"for-each-ref", "--format=%(refname)%00%(objectname)%00%(parent)%00%(contents:subject)", repo.refPrefix,
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	var snapshots []snapshot
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		record, err := parseSnapshotRecord(repo, strings.Split(strings.TrimSuffix(line, "\r"), "\x00"))
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, record)
	}
	return snapshots, nil
}

func newerSnapshot(left, right snapshot) bool {
	if left.ticks != right.ticks {
		return left.ticks > right.ticks
	}
	return left.Name > right.Name
}

func sortedSnapshots(snapshots []snapshot) []snapshot {
	result := append([]snapshot(nil), snapshots...)
	sort.Slice(result, func(left, right int) bool { return newerSnapshot(result[left], result[right]) })
	return result
}

func resolveSnapshot(snapshots []snapshot, name string, head headState) (snapshot, error) {
	var selected snapshot
	found := false
	latest := strings.EqualFold(name, "latest")
	for _, candidate := range snapshots {
		matches := candidate.Name == name
		if latest {
			matches = candidate.Branch == head.branch && (head.branch != "" || candidate.Head == head.head)
		}
		if matches && (!found || newerSnapshot(candidate, selected)) {
			selected, found = candidate, true
		}
	}
	if found {
		return selected, nil
	}
	if latest {
		return snapshot{}, fmt.Errorf("No snapshots for '%s' in this worktree. Run 'snapshot' first.", head.label)
	}
	return snapshot{}, fmt.Errorf("Snapshot '%s' was not found in this worktree. Run 'snapshot list'.", name)
}

func newSnapshotName(branch string, created time.Time, existing []snapshot) string {
	label := strings.Trim(branchLabelPattern.ReplaceAllString(branch, "-"), "-")
	if label == "" {
		label = "branch"
	}
	if len(label) > 64 {
		label = label[:64]
	}
	base := label + "-" + created.Format("2006-01-02-T-15-04")
	names := make(map[string]bool, len(existing))
	for _, item := range existing {
		names[strings.ToLower(item.Name)] = true
	}
	candidate := base
	for suffix := 2; names[strings.ToLower(candidate)]; suffix++ {
		candidate = fmt.Sprintf("%s-%d", base, suffix)
	}
	return candidate
}

func (app *application) commit(repo repository, tree, parent, message string) (string, error) {
	environment := map[string]string{
		"GIT_AUTHOR_NAME": "Git Snapshot", "GIT_AUTHOR_EMAIL": "git-snapshot@localhost",
		"GIT_COMMITTER_NAME": "Git Snapshot", "GIT_COMMITTER_EMAIL": "git-snapshot@localhost",
	}
	arguments := []string{"-c", "commit.gpgSign=false", "commit-tree", tree}
	if parent != "" {
		arguments = append(arguments, "-p", parent)
	}
	oid, err := app.text(repo.root, arguments, environment, []byte(message+"\n"))
	if err == nil && !objectIDPattern.MatchString(oid) {
		err = errors.New("git commit-tree returned an invalid object ID.")
	}
	return oid, err
}

func (app *application) save(repo repository, includeIgnored bool) (string, error) {
	var name string
	err := withSnapshotLock(repo, func() error {
		captured, err := app.capture(repo, true, includeIgnored, "")
		if err != nil {
			return err
		}
		created := time.Now()
		snapshots, err := app.snapshots(repo)
		if err != nil {
			return err
		}
		name = newSnapshotName(captured.head.label, created, snapshots)
		indexCommit, err := app.commit(repo, captured.indexTree, captured.head.head, "git-snapshot-v1 index")
		if err != nil {
			return err
		}
		_, offset := created.Zone()
		record := metadata{
			Version: 1, Name: name, Branch: captured.head.branch, Head: captured.head.head,
			CreatedUTCTicks: strconv.FormatInt(utcTicks(created), 10), UTCOffsetMinutes: offset / 60,
			WorktreeID: repo.worktreeID, IncludeIgnored: includeIgnored, IndexCommit: indexCommit,
		}
		message, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encode snapshot metadata: %w", err)
		}
		commit, err := app.commit(repo, captured.workingTree, indexCommit, metadataPrefix+string(message))
		if err != nil {
			return err
		}
		if err := app.assertSourceUnchanged(repo, captured); err != nil {
			return err
		}
		_, err = app.git(repo.root, []string{"update-ref", repo.refPrefix + name, commit, strings.Repeat("0", len(commit))}, nil, nil)
		return err
	})
	return name, err
}

func showList(output io.Writer, snapshots []snapshot) error {
	if len(snapshots) == 0 {
		_, err := fmt.Fprintln(output, "No snapshots in this worktree. Run 'snapshot' to create one.")
		return err
	}
	width := 4
	for _, item := range snapshots {
		if len(item.Name) > width {
			width = len(item.Name)
		}
	}
	if _, err := fmt.Fprintf(output, "%-*s  %-26s  %s\n", width, "NAME", "CREATED (local)", "BRANCH"); err != nil {
		return err
	}
	for _, item := range sortedSnapshots(snapshots) {
		branch := item.Branch
		if branch == "" {
			branch = "detached-" + item.Head[:12]
		}
		if _, err := fmt.Fprintf(output, "%-*s  %s  %s\n", width, item.Name,
			item.created.Format("2006-01-02 15:04:05 -07:00"), branch); err != nil {
			return err
		}
	}
	return nil
}

func (app *application) compare(repo repository, opts options, snapshots []snapshot, head headState) (int, error) {
	leftName := "latest"
	if len(opts.names) > 0 {
		leftName = opts.names[0]
	}
	left, err := resolveSnapshot(snapshots, leftName, head)
	if err != nil {
		return 0, err
	}
	leftObject := left.oid
	if opts.index {
		leftObject = left.IndexCommit
	}
	var rightObject string
	if len(opts.names) == 2 {
		right, err := resolveSnapshot(snapshots, opts.names[1], head)
		if err != nil {
			return 0, err
		}
		rightObject = right.oid
		if opts.index {
			rightObject = right.IndexCommit
		}
	} else {
		err := withSnapshotLock(repo, func() error {
			current, err := app.capture(repo, !opts.index, left.IncludeIgnored, left.oid)
			if err != nil {
				return err
			}
			rightObject = current.workingTree
			if opts.index {
				rightObject = current.indexTree
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	arguments := append([]string{"diff", "--no-ext-diff", "--no-textconv", "--find-renames"}, opts.diffOptions...)
	arguments = append(arguments, leftObject, rightObject, "--")
	arguments = append(arguments, opts.paths...)
	allowed := []int{0}
	for _, option := range opts.diffOptions {
		if option == "--exit-code" {
			allowed = append(allowed, 1)
			break
		}
	}
	result, err := app.git(repo.invocationDirectory, arguments, nil, nil, allowed...)
	if err != nil {
		return 0, err
	}
	if _, err := app.output.Write(result.output); err != nil {
		return 0, fmt.Errorf("write diff output: %w", err)
	}
	return result.exitCode, nil
}

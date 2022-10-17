package apparmor_test

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/termie/go-shutil"
	"github.com/ubuntu/adsys/internal/policies/apparmor"
	"github.com/ubuntu/adsys/internal/policies/entry"
	"github.com/ubuntu/adsys/internal/testutils"
	"golang.org/x/exp/slices"
)

var update bool

func TestApplyPolicy(t *testing.T) {
	t.Parallel()

	defaultMachineProfile := []entry.Entry{{Key: "apparmor-machine", Value: "usr.bin.foo"}}

	tests := map[string]struct {
		entries []entry.Entry
		user    bool

		noParserOutput         bool
		destAlreadyExists      string
		readOnlyApparmorDir    string
		noApparmorParser       bool
		existingLoadedPolicies []string

		saveAssetsError         bool
		removeUnusedAssetsError bool
		apparmorParserError     string

		wantErr bool
	}{
		// computer cases
		"computer, one profile":                    {},
		"computer, multiple profiles,":             {entries: []entry.Entry{{Key: "apparmor-machine", Value: "usr.bin.foo\nusr.bin.bar\nnested/usr.bin.baz"}}},
		"computer, duplicated profiles":            {entries: []entry.Entry{{Key: "apparmor-machine", Value: "usr.bin.foo\nusr.bin.foo"}}},
		"computer, blank line profiles":            {entries: []entry.Entry{{Key: "apparmor-machine", Value: "usr.bin.foo\n\nusr.bin.bar\n"}}},
		"computer, profiles with whitespace":       {entries: []entry.Entry{{Key: "apparmor-machine", Value: " usr.bin.foo\n\n usr.bin.bar   \nnested/usr.bin.baz "}}},
		"computer, whitespace-only value":          {entries: []entry.Entry{{Key: "apparmor-machine", Value: "       "}}, noParserOutput: true},
		"computer, only blank profiles":            {entries: []entry.Entry{{Key: "apparmor-machine", Value: "\n\n\n"}}, noParserOutput: true},
		"computer, previous profiles are unloaded": {destAlreadyExists: "machine", existingLoadedPolicies: []string{"/usr/bin/foo", "/usr/bin/bar", "/usr/bin/baz"}},
		"existing .old directory is removed":       {destAlreadyExists: "machine.old", noParserOutput: true},
		"existing .new directory is removed":       {destAlreadyExists: "machine.new", noParserOutput: true},

		// shared cases
		"no profiles, existing rules are removed": {entries: []entry.Entry{}, destAlreadyExists: "machine", existingLoadedPolicies: []string{"/usr/bin/foo", "/usr/bin/bar", "/usr/bin/baz"}},
		"no profiles, apparmor directory absent":  {entries: []entry.Entry{}, noParserOutput: true},
		"unexpected entry key":                    {entries: []entry.Entry{{Key: "apparmor-foo", Value: "usr.bin.foo"}}, noParserOutput: true},

		// user cases
		"user, one profile": {entries: []entry.Entry{{Key: "apparmor-user", Value: "usr.bin.foo"}}, user: true, noParserOutput: true},

		// other edge cases
		"no apparmor_parser and no entries":       {entries: []entry.Entry{}, noApparmorParser: true, noParserOutput: true},
		"no apparmor_parser and entries":          {noApparmorParser: true, noParserOutput: true, wantErr: true},
		"read-only root directory and no entries": {entries: []entry.Entry{}, readOnlyApparmorDir: ".", noParserOutput: true},

		// error cases
		"error on loading profiles failing":                {apparmorParserError: "-r", wantErr: true},
		"error on preprocessing new profiles failing":      {apparmorParserError: "-N", wantErr: true},
		"error on preprocessing old profiles failing":      {destAlreadyExists: "machine", existingLoadedPolicies: []string{"/usr/bin/foo"}, apparmorParserError: "-N", wantErr: true},
		"error on unloading all profiles failing":          {entries: []entry.Entry{}, destAlreadyExists: "machine", existingLoadedPolicies: []string{"/usr/bin/foo", "/usr/bin/bar", "/usr/bin/baz"}, apparmorParserError: "-R", wantErr: true},
		"error on unloading old profiles failing":          {destAlreadyExists: "machine", existingLoadedPolicies: []string{"/usr/bin/bar", "/usr/bin/baz"}, apparmorParserError: "-R", wantErr: true},
		"error on save assets dumping failing":             {noParserOutput: true, saveAssetsError: true, wantErr: true},
		"error on removing unused assets after dump":       {noParserOutput: true, removeUnusedAssetsError: true, wantErr: true},
		"error on profile being a directory":               {entries: []entry.Entry{{Key: "apparmor-machine", Value: "nested/"}}, noParserOutput: true, wantErr: true},
		"error on absent profile":                          {entries: []entry.Entry{{Key: "apparmor-machine", Value: "usr.bin.nonexistent"}}, noParserOutput: true, wantErr: true},
		"error on absent loaded policies file":             {entries: []entry.Entry{}, destAlreadyExists: "machine", existingLoadedPolicies: []string{"parseError"}, noParserOutput: true, wantErr: true},
		"error on file as a directory":                     {entries: []entry.Entry{{Key: "apparmor-machine", Value: "usr.bin.foo/notadir"}}, noParserOutput: true, wantErr: true},
		"error on read-only root directory with entries":   {readOnlyApparmorDir: ".", noParserOutput: true, wantErr: true},
		"error on read-only machine directory":             {destAlreadyExists: "machine", readOnlyApparmorDir: "machine", noParserOutput: true, wantErr: true},
		"error on read-only machine directory, no entries": {entries: []entry.Entry{}, destAlreadyExists: "machine", readOnlyApparmorDir: "machine/nested", noParserOutput: true, wantErr: true},
		"error on read-only .old directory":                {destAlreadyExists: "machine.old", readOnlyApparmorDir: "machine.old", noParserOutput: true, wantErr: true},
		"error on read-only .new directory":                {destAlreadyExists: "machine.new", readOnlyApparmorDir: "machine.new", noParserOutput: true, wantErr: true},
	}

	for name, tc := range tests {
		tc := tc
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tc.entries == nil {
				tc.entries = defaultMachineProfile
			}

			apparmorDir := t.TempDir()
			parserCmdOutputFile := filepath.Join(t.TempDir(), "parser-output")
			loadedPoliciesFile := mockLoadedPoliciesFile(t, tc.existingLoadedPolicies)
			if slices.Contains(tc.existingLoadedPolicies, "parseError") {
				loadedPoliciesFile = "not-a-file"
			}
			apparmorParserCmd := mockApparmorParserCmd(t, parserCmdOutputFile)
			if tc.noApparmorParser {
				apparmorParserCmd = []string{"this-definitely-does-not-exist"}
			}
			if tc.apparmorParserError != "" {
				// Let the mock know we want an error on a specific argument
				apparmorParserCmd = append(apparmorParserCmd, fmt.Sprintf("-Exit1%s", tc.apparmorParserError))
			}

			object := "machine"
			if tc.user {
				object = "users"
			}

			if tc.destAlreadyExists != "" {
				require.NoError(t, os.RemoveAll(apparmorDir), "Setup: can't remove apparmor dir before filing it")
				require.NoError(t,
					shutil.CopyTree(
						filepath.Join("testdata", "apparmor_dir", object), filepath.Join(apparmorDir, tc.destAlreadyExists),
						&shutil.CopyTreeOptions{Symlinks: true, CopyFunction: shutil.Copy}),
					"Setup: can't create initial apparmor dir profiles content")
			}
			if tc.readOnlyApparmorDir != "" {
				testutils.MakeReadOnly(t, filepath.Join(apparmorDir, tc.readOnlyApparmorDir))
			}
			mockAssetsDumper := testutils.MockAssetsDumper{Err: tc.saveAssetsError, ReadOnlyErr: tc.removeUnusedAssetsError, Path: "apparmor/", T: t}

			m := apparmor.New(apparmorDir,
				apparmor.WithApparmorParserCmd(apparmorParserCmd),
				apparmor.WithLoadedPoliciesFile(loadedPoliciesFile),
			)
			err := m.ApplyPolicy(context.Background(), "ubuntu", !tc.user, tc.entries, mockAssetsDumper.SaveAssetsTo)
			if tc.wantErr {
				// We don't return here as we want to check that the apparmor
				// dir is in the expected state even in error cases
				require.Error(t, err, "ApplyPolicy should have failed but didn't")
			} else {
				require.NoError(t, err, "ApplyPolicy failed but shouldn't have")
			}

			// Restore permissions to be able to correctly compare trees
			if tc.readOnlyApparmorDir != "" {
				// nolint:gosec //false positive, this is a directory
				err = os.Chmod(filepath.Join(apparmorDir, tc.readOnlyApparmorDir), 0700)
				require.NoError(t, err, "Setup: can't chmod apparmor dir")
			}

			// Restore permissions to the dumped apparmor directory
			if tc.removeUnusedAssetsError {
				err = filepath.WalkDir(filepath.Join(apparmorDir), func(path string, d fs.DirEntry, err error) error {
					require.NoError(t, err, "Setup: can't walk dumped apparmor dir")
					if d.IsDir() {
						// nolint:gosec //false positive, this is a directory
						err = os.Chmod(path, 0700)
					} else {
						err = os.Chmod(path, 0600)
					}
					require.NoError(t, err, "Setup: can't chmod path")
					return nil
				})
				require.NoError(t, err, "Setup: can't restore permissions of dumped files")
			}
			testutils.CompareTreesWithFiltering(t, apparmorDir, filepath.Join("testdata", "golden", testutils.NormalizeGoldenName(t, t.Name()), "etc", "apparmor.d", "adsys"), update)

			// Return early if we don't want to check apparmor_parser output for
			// whatever reason (e.g. command did not execute, returned an error before etc.)
			if tc.noParserOutput {
				return
			}

			// Check that apparmor_parser was called with the expected arguments
			goldPath := filepath.Join("testdata", "golden", testutils.NormalizeGoldenName(t, t.Name()), fmt.Sprintf("parser_output-%s", userOrMachine(tc.user)))
			got, err := os.ReadFile(parserCmdOutputFile)
			require.NoError(t, err, "Setup: Can't read parser output file")
			got = []byte(normalizeOutput(t, string(got), apparmorDir))
			if update {
				err = os.WriteFile(goldPath, got, 0600)
				require.NoError(t, err, "Setup: Can't write golden file")
			}
			want, err := os.ReadFile(goldPath)
			require.NoError(t, err, "Setup: Can't read golden file")
			require.Equal(t, string(want), string(got), "Apparmor parser command output doesn't match")
		})
	}
}

func appendToFile(t *testing.T, path string, data []byte) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	require.NoError(t, err, "Setup: Can't open file for appending")
	defer f.Close()

	_, err = f.Write(data)
	require.NoError(t, err, "Setup: Can't write to file")
}

func mockApparmorParserCmd(t *testing.T, parserOutputFile string, args ...string) []string {
	t.Helper()

	cmdArgs := []string{"env", "GO_WANT_HELPER_PROCESS=1", os.Args[0], "-test.run=TestMockApparmorParser", "--", parserOutputFile}
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}

func TestMockApparmorParser(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	var callParser bool
	var outputFile string
	var unloadedPolicies []string
	var wantExit string

	args := os.Args
	for len(args) > 0 {
		if args[0] != "--" {
			args = args[1:]
			continue
		}
		// First arg after -- is the output file to write to
		outputFile = args[1]
		// Args are shifted by 1 if exit was requested
		// Remove the actual -Exit- argument in case we want to execute the
		// underlying command
		if strings.HasPrefix(args[2], "-Exit1-") {
			wantExit = args[2]
			args = args[3:]
			break
		}
		args = args[2:]
		break
	}

	// Handle specific apparmor_parser flags
	switch args[0] {
	case "-N":
		// -N is an unprivileged call to apparmor_parser, so it's safe to
		// call the command ourselves and register its output
		callParser = true
	case "-R":
		// Calls to remove policies contain the policy names on stdin, which
		// we read here and subsequently append to the parser file
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			unloadedPolicies = append(unloadedPolicies, scanner.Text())
		}
		require.NoError(t, scanner.Err(), "Setup: Can't read from stdin")
	}

	// Dump the newline-separated args to the output file, appending if needed
	// in order to track multiple apparmor_parser invocations
	if wantExit != "" {
		appendToFile(t, outputFile, []byte(wantExit+"\n"))
	}
	appendToFile(t, outputFile, []byte(strings.Join(args, "\n")+"\n"))

	// Dump any policies that were unloaded to the output file
	if len(unloadedPolicies) > 0 {
		// Sort the policies to make the output deterministic in parallel testing
		sort.Strings(unloadedPolicies)
		appendToFile(t, outputFile, []byte(strings.Join(unloadedPolicies, "\n")+"\n"))
	}

	// Call the real apparmor_parser command if taking an unprivileged action
	if callParser {
		// #nosec G204 - We are in control of the arguments in tests
		cmd := exec.Command("apparmor_parser", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		require.NoError(t, err, "Setup: Calling apparmor_parser -N failed")
	}

	if wantExit != "" {
		// Only exit on the requested argument
		if strings.HasSuffix(wantExit, args[0]) {
			fmt.Println("EXIT 1 requested in mock")
			os.Exit(1)
		}
	}
}

func normalizeOutput(t *testing.T, out string, tmpPath string) string {
	t.Helper()

	return strings.ReplaceAll(out, tmpPath, "#TMPDIR#")
}

func userOrMachine(user bool) string {
	if user {
		return "user"
	}
	return "machine"
}

func mockLoadedPoliciesFile(t *testing.T, policies []string) string {
	t.Helper()

	// The contents of this file are cross-referenced with the apparmor.d/adsys
	// directory structure in order to determine which of the policies are loaded.
	path := filepath.Join(t.TempDir(), "loaded-policies")
	err := os.WriteFile(path, []byte(strings.Join(policies, " (enforce)\n")+" (enforce)\n"), 0600)
	require.NoError(t, err, "Setup: Can't write loaded policies file")
	return path
}

func TestMain(m *testing.M) {
	flag.BoolVar(&update, "update", false, "update golden files")
	flag.Parse()

	m.Run()
}
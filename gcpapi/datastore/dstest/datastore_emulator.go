package dstest

// This file is responsible for starting and managing the datastore emulator
// (see package doc); the main entrypoint is acquireDatastoreEmulator (as well
// as the DatastoreEmulator type).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Khan/districts-jobs/pkg/errors"
)

// DatastoreEmulator keeps track of details of a datastore emulator
// running in a separate process.
type DatastoreEmulator struct {
	Addr string `json:"addr"`
	Pid  int    `json:"pid"`
	// Currently this field is unused, but including it here to make it
	// easier to implement emulator timeouts in the future.
	LockExpirationTime time.Time `json:"lockExpirationTime"`
	// lockfile is not exported so that `json.Marshal` won't include it
	// when serializing
	lockFile    *os.File
	LogFilename string `json:"logFilename"`
}

func gitCommandWithBasePath(out io.Writer, basePath string, cmds []string) error {
	return CommandWithBasePath("git", out, basePath, cmds)
}

func GitRepoLocalRoot(basepath string) (string, error) {
	var buf bytes.Buffer
	err := gitCommandWithBasePath(&buf, basepath, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return "", errors.WrapWithFields(err, errors.Fields{"git-rev-parse-output": buf.String()})
	}
	return strings.TrimSpace(buf.String()), nil
}

func getWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = os.Getenv("PWD") // not as reliable, but can't error!
	}
	return cwd
}

func CommandWithBasePath(command string, out io.Writer, basePath string, cmds []string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command(fmt.Sprintf("%s.exe", command), cmds...)
	case "linux", "darwin":
		cmd = exec.Command(command, cmds...)
	default:
		return errors.New("unsupported platform")
	}
	cmd.Dir = basePath
	// for verbose output
	// log.Println(command, cmds)

	cmd.Stdin = os.Stdin
	if out != nil {
		cmd.Stdout = out
		cmd.Stderr = out
	}

	return cmd.Run()
}

var lockDirAbsPath string

func LockDirPath() string {
	if lockDirAbsPath != "" {
		return lockDirAbsPath
	}
	wd := getWD()
	repoRoot, err := GitRepoLocalRoot(wd)
	if err != nil {
		panic(err)
	}
	lockDirAbsPath = filepath.Join(repoRoot, "pkg/gcpapi/datastore/dstest/lockfiles")
	return lockDirAbsPath
}

var emulatorUnavailable = "This particular emulator is unavailable"

func (emulator *DatastoreEmulator) datadir() string {
	return strings.Replace(emulator.LogFilename, ".out", ".data", 1)
}

// Reset resets the datastore emulator back to empty.
//
// It can be useful to call this before each test to ensure no state
// leaks between test cases.
func (emulator *DatastoreEmulator) Reset(ctx context.Context) error {
	// The /reset endpoint isn't officially documented, but it seems to
	// be relatively stable.
	//
	// To allow for parallel testing using the same emulator, we could
	// just define a new project id per test case instead of clearing
	// out old data. This would mean having test cases build a new
	// dsClient for each test case, since we cannot change the project id
	// per test
	url := fmt.Sprintf("http://%v/reset", emulator.Addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return errors.Service("Error resetting datastore emulator", err)
	}

	// TODO(benkraft): Refactor to pass in http-context and use ctx.HTTP().
	//nolint:ka-banned-symbol // see previous line
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.Service("Error resetting datastore emulator", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.Service(
			"Invalid status code resetting datastore emulator",
			errors.Fields{"statusCode": resp.StatusCode})
	}
	return nil
}

// Release releases the lock on this DatastoreEmulator, allowing other
// test suites (or other go test processes) to use it.  It also does
// some final "tear-down" sanity checking, such as checking that the
// test did not use any invalid composite datastore indexes.
func (emulator *DatastoreEmulator) Release() error {
	missing, err := missingCompositeIndexes(emulator.datadir())
	if err != nil {
		return err
	}
	if missing != "" {
		return errors.Internal(
			"Test uses composite indexes that are missing from index.yaml (and "+
				"Go datastore queries should always have perfect indexes).",
			errors.Fields{"indexes": missing})
	}

	err = syscall.Flock(int(emulator.lockFile.Fd()), syscall.LOCK_UN)
	if err != nil {
		err = errors.Service("unable to release emulator",
			err,
			errors.Fields{
				"filename": emulator.lockFile.Name(),
				"fd":       emulator.lockFile.Fd(),
			})
	}

	emulator.lockFile.Close()
	return err
}

func acquireDatastoreEmulator(ctx context.Context, projectID string) (*DatastoreEmulator, error) {
	// First we try to lock an emulator that's already running.
	emulator, err := lockRunningEmulator(ctx)
	if err != nil && !errors.Is(err, errors.TransientKhanServiceKind) {
		return nil, errors.Wrap(err, "unable to lock emulator")
	}

	if emulator == nil {
		emulator, err = startEmulator(ctx, projectID)
		if err != nil {
			return nil, errors.Wrap(err, "unable to start new emulator")
		}
	} else {
		// We got an emulator.  Make sure it's clean before use.
		err = emulator.Reset(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "unable to reset emulator")
		}
	}

	// Clear out the index.xml file from an old test, so it doesn't
	// mess up our composite-index analysis in Release().  Also make
	// sure the index.xml file exists (which is why we call it at
	// the start of using the emulator-dir rather than the end).
	clearIndexXMLFile(emulator.datadir())

	return emulator, nil
}

func lockRunningEmulator(ctx context.Context) (*DatastoreEmulator, error) {
	lockDirPath := LockDirPath()
	files, err := ioutil.ReadDir(lockDirPath)
	// If we can't read the directory it may not exist - we'll create it
	// later when we start a new emulator
	if err != nil {
		fmt.Println("Lockfile Directory does not exist", err)
		return nil, errors.TransientKhanService(err, "message", "Lockfile Directory does not exist")
	}

	for _, fileinfo := range files {
		if !strings.HasSuffix(fileinfo.Name(), ".json") {
			continue
		}
		filePath := filepath.Join(lockDirPath, fileinfo.Name())
		emulator, err := tryLockEmulator(ctx, filePath)
		if err != nil {
			fmt.Println("tryLockEmulator failed", err)
			continue
		}
		return emulator, nil
	}

	// If we reach this point - none of the lock files were available
	return nil, errors.TransientKhanService(err, "message", "no lock files available")
}

func tryLockEmulator(ctx context.Context, filePath string) (*DatastoreEmulator, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, errors.Internal("Error trying to open file", err,
			errors.Fields{"filePath": filePath})
	}
	defer func() {
		if err != nil {
			file.Close()
		}
	}()

	// Try to flock the file, but don't block if another process has it
	// already
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		fmt.Println("syscall.EWOULDBLOCK ", filePath, err)
		return nil, errors.Service(err, "message", emulatorUnavailable)
	} else if err != nil {
		fmt.Println("Tried to flock the file, but not block if another process has it already", filePath, err)
		return nil, errors.Internal("Error trying to flock file", err,
			errors.Fields{"filePath": filePath})
	}

	// Now that we have the lock, read it and make sure it's still valid
	emulator, err := emulatorFromFile(ctx, file)
	// If the process isn't alive, delete the lock file and the
	// associated log file, and proceed with any remaining ones.
	if err != nil {
		os.Remove(filePath)
		os.Remove(strings.Replace(filePath, ".lockfile.json", ".out", 1))
		fmt.Println("The process isn't alive", file, err)
		return nil, errors.Service(err, "message", emulatorUnavailable)
	}

	return emulator, nil
}

func emulatorFromFile(ctx context.Context, lockedFile *os.File) (*DatastoreEmulator, error) {
	// Read the lock file and check that the process is still running
	jsonData, err := ioutil.ReadAll(lockedFile)
	if err != nil {
		return nil, errors.Internal("Could not read lockfile", err)
	}

	var emulator DatastoreEmulator
	err = json.Unmarshal(jsonData, &emulator)
	if err != nil {
		return nil, errors.Internal("Could not unmarshal lockfile", err)
	}

	if emulator.Pid == 0 || emulator.Addr == "" {
		return nil, errors.Internal("Lockfile contains invalid data")
	}
	emulator.lockFile = lockedFile

	// Check that the process is still alive by sending the process a fake
	// signal (signum = 0). See `man 2 kill` or
	// https://unix.stackexchange.com/questions/169898/what-does-kill-0-do for
	// more details
	err = syscall.Kill(emulator.Pid, syscall.Signal(0))
	if err != nil {
		return nil, errors.Internal("Process no longer running",
			errors.Fields{"pid": emulator.Pid})
	}

	// In case the process is stuck, or is dead but another process is using
	// the same pid, we also check we can ping it.  This should be quick in the
	// common case where the process is in fact running.
	logName := emulator.LogFilename
	if logName == "" {
		// TODO(benkraft): Remove this case after we've been writing
		// LogFilename for a while (definitely by Feb 2020).
		logName = strings.Replace(
			lockedFile.Name(), ".lockfile.json", ".out", 1)
	}
	err = waitForStartup(ctx, emulator.Addr, logName)
	if err != nil {
		fmt.Println("waitForStartup got error:", err)
		// caller will remove the lockfile on error
		return nil, errors.Internal("Could not contact emulator",
			err, errors.Fields{"addr": emulator.Addr})
	}

	return &emulator, nil
}

func startEmulator(ctx context.Context, projectID string) (*DatastoreEmulator, error) {
	lockDirPath := LockDirPath()

	// First find a free port to run the emulator on
	// TODO(dhruv): Make this more robust by retrying to find a port 3 times
	// before failing.
	emulatorPort, err := findFreePort()
	if err != nil {
		fmt.Println("findFreePort got error", emulatorPort)
		return nil, errors.Internal("Could not find a free port to start emulator", err)
	}

	emulatorAddr := fmt.Sprintf("localhost:%v", emulatorPort)

	err = os.MkdirAll(lockDirPath, 0o777)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	gcloudOutput, err := ioutil.TempFile(lockDirPath, "emulator-*.out")
	if err != nil {
		return nil, errors.WithStack(err)
	}
	// Close the file if any error occurred.  Note we don't delete it,
	// since having it around can help with debugging.
	defer func() {
		if err != nil {
			gcloudOutput.Close()
		}
	}()

	// Start the emulator on that port
	// TODO(dhruv): Consider adding a timeout here if we find it's too
	// resource intensive to constantly run an emulator for testing.
	cmdPath, err := exec.LookPath("gcloud")
	if err != nil {
		return nil, errors.Internal("Could not find gcloud executable", err)
	}

	args := []string{
		"beta", "emulators", "datastore", "start",
		"--project=" + projectID,
		"--host-port=" + emulatorAddr,
		"--data-dir=" + strings.Replace(gcloudOutput.Name(), ".out", ".data", 1),
		// We must pass `--no-store-on-disk` for /reset to work.
		"--no-store-on-disk",
		"--consistency=1",
	}
	cmd := exec.Command(cmdPath, args...)
	cmd.Stdout = gcloudOutput
	cmd.Stderr = gcloudOutput

	err = cmd.Start()
	if err != nil {
		fmt.Println("starting emulator got error:", err)
		return nil, errors.WrapWithFields(err,
			errors.Fields{"emulator_cmd": fmt.Sprintf("%s %s", cmdPath, strings.Join(args, " "))})
	}

	err = waitForStartup(ctx, emulatorAddr, gcloudOutput.Name())
	if err != nil {
		return nil, errors.WrapWithFields(err,
			errors.Fields{
				"emulator_cmd": fmt.Sprintf("%s %s",
					cmdPath, strings.Join(args, " ")),
			})
	}

	lockfilePath := strings.Replace(gcloudOutput.Name(), ".out", ".lockfile.json", 1)

	// Now that we have a valid emulator, write its config to a lockfile
	// to be used by other processes when ours exits.
	lockFile, err := os.Create(lockfilePath)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	// Delete and close the file if any error occurred.  We need to hold
	// it open otherwise, since our Flock depends on an open file
	// descriptor.
	defer func() {
		if err != nil {
			lockFile.Close()
			os.Remove(lockfilePath)
		}
	}()

	err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	emulator := DatastoreEmulator{
		Addr:        emulatorAddr,
		Pid:         cmd.Process.Pid,
		LogFilename: gcloudOutput.Name(),
		lockFile:    lockFile,
	}

	emulatorData, err := json.Marshal(&emulator)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	_, err = lockFile.Write(emulatorData)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &emulator, nil
}

func findFreePort() (int, error) {
	// Create a tcp listener on an open port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, errors.WithStack(err)
	}

	// Figure out the port that was assigned
	port := listener.Addr().(*net.TCPAddr).Port

	// Close the listener so the port will be availble for our use.
	err = listener.Close()

	// NOTE: Since we aren't locking the port it's not guaranteed to be
	// free for our process to use. To be safer we could start our process
	// with a hard coded port and then retry on a separate port if that
	// initial process fails.
	return port, errors.WithStack(err)
}

const (
	startupTimeout  = 100 * time.Second
	pollingInterval = 100 * time.Millisecond
)

func waitForStartup(ctx context.Context, addr string, logfileName string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	defer func() {
		// Attach the logs to the error message.
		if err != nil {
			emulatorOutput := []byte("<unknown>")
			logfile, openErr := os.Open(logfileName)
			if openErr == nil {
				fmt.Println("Unable to open logfile", logfileName, err)
				defer logfile.Close()
				emulatorOutput, _ = ioutil.ReadAll(logfile)
			}
			message := "Error trying to connect to datastore emulator"
			if errors.Is(err, context.DeadlineExceeded) {
				fmt.Println(message, err)
				message = "Timed out trying to connect to datastore emulator"
			}
			err = errors.Internal(message, err, errors.Fields{
				"startupTimeout": startupTimeout,
				"emulatorOutput": string(emulatorOutput),
			})
		}
	}()

	tryAgain, err := checkEmulatorConnection(ctx, addr)
	if !tryAgain {
		return err
	}

	for {
		// TODO(csilvers): change these cases to err != nil instead.
		select {
		case <-time.After(pollingInterval):
			tryAgain, err := checkEmulatorConnection(ctx, addr)
			if !tryAgain {
				return err
			}
		case <-ctx.Done():
			err = ctx.Err()
			return errors.WithStack(err)
		}
	}
}

// checkEmulatorConnection attempts to make an HTTP request to the emulator.
// If it gets the expected response *or* encounters an error condition that
// is abnormal, it returns false for tryAgain. That means it's safe to
// just return err (because tryAgain is false and err is nil if the emulator
// is working).
//
// If it appears that the emulator is just not ready, this will return
// true for tryAgain with a nil error.
func checkEmulatorConnection(ctx context.Context, addr string) (tryAgain bool, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr, nil)
	if err != nil {
		return false, errors.WithStack(err)
	}
	// TODO(benkraft): Refactor to pass in http-context and use ctx.HTTP().
	//nolint:ka-banned-symbol // see previous line
	resp, err := http.DefaultClient.Do(req)

	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return false, errors.Internal("Got wrong status code connecting",
				errors.Fields{"statusCode": resp.StatusCode})
		}
		// No error and status code == 200, success!
		return false, nil
	}
	if !strings.Contains(err.Error(), "connection refused") {
		return false, errors.WithStack(err)
	}
	return true, nil
}

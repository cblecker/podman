package libpod

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v2/libpod/define"
	"github.com/containers/podman/v2/pkg/errorhandling"
	"github.com/containers/podman/v2/pkg/util"
	"github.com/containers/podman/v2/utils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecContainer executes a command in a running container
func (r *ConmonOCIRuntime) ExecContainer(c *Container, sessionID string, options *ExecOptions, streams *define.AttachStreams) (int, chan error, error) {
	if options == nil {
		return -1, nil, errors.Wrapf(define.ErrInvalidArg, "must provide an ExecOptions struct to ExecContainer")
	}
	if len(options.Cmd) == 0 {
		return -1, nil, errors.Wrapf(define.ErrInvalidArg, "must provide a command to execute")
	}

	if sessionID == "" {
		return -1, nil, errors.Wrapf(define.ErrEmptyID, "must provide a session ID for exec")
	}

	// TODO: Should we default this to false?
	// Or maybe make streams mandatory?
	attachStdin := true
	if streams != nil {
		attachStdin = streams.AttachInput
	}

	var ociLog string
	if logrus.GetLevel() != logrus.DebugLevel && r.supportsJSON {
		ociLog = c.execOCILog(sessionID)
	}

	execCmd, pipes, err := r.startExec(c, sessionID, options, attachStdin, ociLog)
	if err != nil {
		return -1, nil, err
	}

	// Only close sync pipe. Start and attach are consumed in the attach
	// goroutine.
	defer func() {
		if pipes.syncPipe != nil && !pipes.syncClosed {
			errorhandling.CloseQuiet(pipes.syncPipe)
			pipes.syncClosed = true
		}
	}()

	// TODO Only create if !detach
	// Attach to the container before starting it
	attachChan := make(chan error)
	go func() {
		// attachToExec is responsible for closing pipes
		attachChan <- c.attachToExec(streams, options.DetachKeys, sessionID, pipes.startPipe, pipes.attachPipe)
		close(attachChan)
	}()

	if err := execCmd.Wait(); err != nil {
		return -1, nil, errors.Wrapf(err, "cannot run conmon")
	}

	pid, err := readConmonPipeData(pipes.syncPipe, ociLog)

	return pid, attachChan, err
}

// ExecContainerHTTP executes a new command in an existing container and
// forwards its standard streams over an attach
func (r *ConmonOCIRuntime) ExecContainerHTTP(ctr *Container, sessionID string, options *ExecOptions, req *http.Request, w http.ResponseWriter, streams *HTTPAttachStreams, cancel <-chan bool, hijackDone chan<- bool, holdConnOpen <-chan bool) (int, chan error, error) {
	if streams != nil {
		if !streams.Stdin && !streams.Stdout && !streams.Stderr {
			return -1, nil, errors.Wrapf(define.ErrInvalidArg, "must provide at least one stream to attach to")
		}
	}

	if options == nil {
		return -1, nil, errors.Wrapf(define.ErrInvalidArg, "must provide exec options to ExecContainerHTTP")
	}

	detachString := config.DefaultDetachKeys
	if options.DetachKeys != nil {
		detachString = *options.DetachKeys
	}
	detachKeys, err := processDetachKeys(detachString)
	if err != nil {
		return -1, nil, err
	}

	// TODO: Should we default this to false?
	// Or maybe make streams mandatory?
	attachStdin := true
	if streams != nil {
		attachStdin = streams.Stdin
	}

	var ociLog string
	if logrus.GetLevel() != logrus.DebugLevel && r.supportsJSON {
		ociLog = ctr.execOCILog(sessionID)
	}

	execCmd, pipes, err := r.startExec(ctr, sessionID, options, attachStdin, ociLog)
	if err != nil {
		return -1, nil, err
	}

	// Only close sync pipe. Start and attach are consumed in the attach
	// goroutine.
	defer func() {
		if pipes.syncPipe != nil && !pipes.syncClosed {
			errorhandling.CloseQuiet(pipes.syncPipe)
			pipes.syncClosed = true
		}
	}()

	attachChan := make(chan error)
	go func() {
		// attachToExec is responsible for closing pipes
		attachChan <- attachExecHTTP(ctr, sessionID, req, w, streams, pipes, detachKeys, options.Terminal, cancel, hijackDone, holdConnOpen)
		close(attachChan)
	}()

	// Wait for conmon to succeed, when return.
	if err := execCmd.Wait(); err != nil {
		return -1, nil, errors.Wrapf(err, "cannot run conmon")
	}

	pid, err := readConmonPipeData(pipes.syncPipe, ociLog)

	return pid, attachChan, err
}

// ExecContainerDetached executes a command in a running container, but does
// not attach to it.
func (r *ConmonOCIRuntime) ExecContainerDetached(ctr *Container, sessionID string, options *ExecOptions, stdin bool) (int, error) {
	if options == nil {
		return -1, errors.Wrapf(define.ErrInvalidArg, "must provide exec options to ExecContainerHTTP")
	}

	var ociLog string
	if logrus.GetLevel() != logrus.DebugLevel && r.supportsJSON {
		ociLog = ctr.execOCILog(sessionID)
	}

	execCmd, pipes, err := r.startExec(ctr, sessionID, options, stdin, ociLog)
	if err != nil {
		return -1, err
	}

	defer func() {
		pipes.cleanup()
	}()

	// Wait for Conmon to tell us we're ready to attach.
	// We aren't actually *going* to attach, but this means that we're good
	// to proceed.
	if _, err := readConmonPipeData(pipes.attachPipe, ""); err != nil {
		return -1, err
	}

	// Start the exec session
	if err := writeConmonPipeData(pipes.startPipe); err != nil {
		return -1, err
	}

	// Wait for conmon to succeed, when return.
	if err := execCmd.Wait(); err != nil {
		return -1, errors.Wrapf(err, "cannot run conmon")
	}

	pid, err := readConmonPipeData(pipes.syncPipe, ociLog)

	return pid, err
}

// ExecAttachResize resizes the TTY of the given exec session.
func (r *ConmonOCIRuntime) ExecAttachResize(ctr *Container, sessionID string, newSize remotecommand.TerminalSize) error {
	controlFile, err := openControlFile(ctr, ctr.execBundlePath(sessionID))
	if err != nil {
		return err
	}
	defer controlFile.Close()

	if _, err = fmt.Fprintf(controlFile, "%d %d %d\n", 1, newSize.Height, newSize.Width); err != nil {
		return errors.Wrapf(err, "failed to write to ctl file to resize terminal")
	}

	return nil
}

// ExecStopContainer stops a given exec session in a running container.
func (r *ConmonOCIRuntime) ExecStopContainer(ctr *Container, sessionID string, timeout uint) error {
	pid, err := ctr.getExecSessionPID(sessionID)
	if err != nil {
		return err
	}

	logrus.Debugf("Going to stop container %s exec session %s", ctr.ID(), sessionID)

	// Is the session dead?
	// Ping the PID with signal 0 to see if it still exists.
	if err := unix.Kill(pid, 0); err != nil {
		if err == unix.ESRCH {
			return nil
		}
		return errors.Wrapf(err, "error pinging container %s exec session %s PID %d with signal 0", ctr.ID(), sessionID, pid)
	}

	if timeout > 0 {
		// Use SIGTERM by default, then SIGSTOP after timeout.
		logrus.Debugf("Killing exec session %s (PID %d) of container %s with SIGTERM", sessionID, pid, ctr.ID())
		if err := unix.Kill(pid, unix.SIGTERM); err != nil {
			if err == unix.ESRCH {
				return nil
			}
			return errors.Wrapf(err, "error killing container %s exec session %s PID %d with SIGTERM", ctr.ID(), sessionID, pid)
		}

		// Wait for the PID to stop
		if err := waitPidStop(pid, time.Duration(timeout)*time.Second); err != nil {
			logrus.Infof("Timed out waiting for container %s exec session %s to stop, resorting to SIGKILL: %v", ctr.ID(), sessionID, err)
		} else {
			// No error, container is dead
			return nil
		}
	}

	// SIGTERM did not work. On to SIGKILL.
	logrus.Debugf("Killing exec session %s (PID %d) of container %s with SIGKILL", sessionID, pid, ctr.ID())
	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		if err == unix.ESRCH {
			return nil
		}
		return errors.Wrapf(err, "error killing container %s exec session %s PID %d with SIGKILL", ctr.ID(), sessionID, pid)
	}

	// Wait for the PID to stop
	if err := waitPidStop(pid, killContainerTimeout*time.Second); err != nil {
		return errors.Wrapf(err, "timed out waiting for container %s exec session %s PID %d to stop after SIGKILL", ctr.ID(), sessionID, pid)
	}

	return nil
}

// ExecUpdateStatus checks if the given exec session is still running.
func (r *ConmonOCIRuntime) ExecUpdateStatus(ctr *Container, sessionID string) (bool, error) {
	pid, err := ctr.getExecSessionPID(sessionID)
	if err != nil {
		return false, err
	}

	logrus.Debugf("Checking status of container %s exec session %s", ctr.ID(), sessionID)

	// Is the session dead?
	// Ping the PID with signal 0 to see if it still exists.
	if err := unix.Kill(pid, 0); err != nil {
		if err == unix.ESRCH {
			return false, nil
		}
		return false, errors.Wrapf(err, "error pinging container %s exec session %s PID %d with signal 0", ctr.ID(), sessionID, pid)
	}

	return true, nil
}

// ExecContainerCleanup cleans up files created when a command is run via
// ExecContainer. This includes the attach socket for the exec session.
func (r *ConmonOCIRuntime) ExecContainerCleanup(ctr *Container, sessionID string) error {
	// Clean up the sockets dir. Issue #3962
	// Also ignore if it doesn't exist for some reason; hence the conditional return below
	if err := os.RemoveAll(filepath.Join(r.socketsDir, sessionID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ExecAttachSocketPath is the path to a container's exec session attach socket.
func (r *ConmonOCIRuntime) ExecAttachSocketPath(ctr *Container, sessionID string) (string, error) {
	// We don't even use container, so don't validity check it
	if sessionID == "" {
		return "", errors.Wrapf(define.ErrInvalidArg, "must provide a valid session ID to get attach socket path")
	}

	return filepath.Join(r.socketsDir, sessionID, "attach"), nil
}

// This contains pipes used by the exec API.
type execPipes struct {
	syncPipe     *os.File
	syncClosed   bool
	startPipe    *os.File
	startClosed  bool
	attachPipe   *os.File
	attachClosed bool
}

func (p *execPipes) cleanup() {
	if p.syncPipe != nil && !p.syncClosed {
		errorhandling.CloseQuiet(p.syncPipe)
		p.syncClosed = true
	}
	if p.startPipe != nil && !p.startClosed {
		errorhandling.CloseQuiet(p.startPipe)
		p.startClosed = true
	}
	if p.attachPipe != nil && !p.attachClosed {
		errorhandling.CloseQuiet(p.attachPipe)
		p.attachClosed = true
	}
}

// Start an exec session's conmon parent from the given options.
func (r *ConmonOCIRuntime) startExec(c *Container, sessionID string, options *ExecOptions, attachStdin bool, ociLog string) (_ *exec.Cmd, _ *execPipes, deferredErr error) {
	pipes := new(execPipes)

	if options == nil {
		return nil, nil, errors.Wrapf(define.ErrInvalidArg, "must provide an ExecOptions struct to ExecContainer")
	}
	if len(options.Cmd) == 0 {
		return nil, nil, errors.Wrapf(define.ErrInvalidArg, "must provide a command to execute")
	}

	if sessionID == "" {
		return nil, nil, errors.Wrapf(define.ErrEmptyID, "must provide a session ID for exec")
	}

	// create sync pipe to receive the pid
	parentSyncPipe, childSyncPipe, err := newPipe()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error creating socket pair")
	}
	pipes.syncPipe = parentSyncPipe

	defer func() {
		if deferredErr != nil {
			pipes.cleanup()
		}
	}()

	// create start pipe to set the cgroup before running
	// attachToExec is responsible for closing parentStartPipe
	childStartPipe, parentStartPipe, err := newPipe()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error creating socket pair")
	}
	pipes.startPipe = parentStartPipe

	// create the attach pipe to allow attach socket to be created before
	// $RUNTIME exec starts running. This is to make sure we can capture all output
	// from the process through that socket, rather than half reading the log, half attaching to the socket
	// attachToExec is responsible for closing parentAttachPipe
	parentAttachPipe, childAttachPipe, err := newPipe()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error creating socket pair")
	}
	pipes.attachPipe = parentAttachPipe

	childrenClosed := false
	defer func() {
		if !childrenClosed {
			errorhandling.CloseQuiet(childSyncPipe)
			errorhandling.CloseQuiet(childAttachPipe)
			errorhandling.CloseQuiet(childStartPipe)
		}
	}()

	runtimeDir, err := util.GetRuntimeDir()
	if err != nil {
		return nil, nil, err
	}

	finalEnv := make([]string, 0, len(options.Env))
	for k, v := range options.Env {
		finalEnv = append(finalEnv, fmt.Sprintf("%s=%s", k, v))
	}

	processFile, err := prepareProcessExec(c, options, finalEnv, sessionID)
	if err != nil {
		return nil, nil, err
	}

	args := r.sharedConmonArgs(c, sessionID, c.execBundlePath(sessionID), c.execPidPath(sessionID), c.execLogPath(sessionID), c.execExitFileDir(sessionID), ociLog, define.NoLogging, "")

	if options.PreserveFDs > 0 {
		args = append(args, formatRuntimeOpts("--preserve-fds", fmt.Sprintf("%d", options.PreserveFDs))...)
	}

	for _, capability := range options.CapAdd {
		args = append(args, formatRuntimeOpts("--cap", capability)...)
	}

	if options.Terminal {
		args = append(args, "-t")
	}

	if attachStdin {
		args = append(args, "-i")
	}

	// Append container ID and command
	args = append(args, "-e")
	// TODO make this optional when we can detach
	args = append(args, "--exec-attach")
	args = append(args, "--exec-process-spec", processFile.Name())

	if len(options.ExitCommand) > 0 {
		args = append(args, "--exit-command", options.ExitCommand[0])
		for _, arg := range options.ExitCommand[1:] {
			args = append(args, []string{"--exit-command-arg", arg}...)
		}
		if options.ExitCommandDelay > 0 {
			args = append(args, []string{"--exit-delay", fmt.Sprintf("%d", options.ExitCommandDelay)}...)
		}
	}

	logrus.WithFields(logrus.Fields{
		"args": args,
	}).Debugf("running conmon: %s", r.conmonPath)
	execCmd := exec.Command(r.conmonPath, args...)

	// TODO: This is commented because it doesn't make much sense in HTTP
	// attach, and I'm not certain it does for non-HTTP attach as well.
	// if streams != nil {
	// 	// Don't add the InputStream to the execCmd. Instead, the data should be passed
	// 	// through CopyDetachable
	// 	if streams.AttachOutput {
	// 		execCmd.Stdout = options.Streams.OutputStream
	// 	}
	// 	if streams.AttachError {
	// 		execCmd.Stderr = options.Streams.ErrorStream
	// 	}
	// }

	conmonEnv, extraFiles := r.configureConmonEnv(c, runtimeDir)

	var filesToClose []*os.File
	if options.PreserveFDs > 0 {
		for fd := 3; fd < int(3+options.PreserveFDs); fd++ {
			f := os.NewFile(uintptr(fd), fmt.Sprintf("fd-%d", fd))
			filesToClose = append(filesToClose, f)
			execCmd.ExtraFiles = append(execCmd.ExtraFiles, f)
		}
	}

	// we don't want to step on users fds they asked to preserve
	// Since 0-2 are used for stdio, start the fds we pass in at preserveFDs+3
	execCmd.Env = r.conmonEnv
	execCmd.Env = append(execCmd.Env, fmt.Sprintf("_OCI_SYNCPIPE=%d", options.PreserveFDs+3), fmt.Sprintf("_OCI_STARTPIPE=%d", options.PreserveFDs+4), fmt.Sprintf("_OCI_ATTACHPIPE=%d", options.PreserveFDs+5))
	execCmd.Env = append(execCmd.Env, conmonEnv...)

	execCmd.ExtraFiles = append(execCmd.ExtraFiles, childSyncPipe, childStartPipe, childAttachPipe)
	execCmd.ExtraFiles = append(execCmd.ExtraFiles, extraFiles...)
	execCmd.Dir = c.execBundlePath(sessionID)
	execCmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	err = startCommandGivenSelinux(execCmd)

	// We don't need children pipes  on the parent side
	errorhandling.CloseQuiet(childSyncPipe)
	errorhandling.CloseQuiet(childAttachPipe)
	errorhandling.CloseQuiet(childStartPipe)
	childrenClosed = true

	if err != nil {
		return nil, nil, errors.Wrapf(err, "cannot start container %s", c.ID())
	}
	if err := r.moveConmonToCgroupAndSignal(c, execCmd, parentStartPipe); err != nil {
		return nil, nil, err
	}

	// These fds were passed down to the runtime.  Close them
	// and not interfere
	for _, f := range filesToClose {
		errorhandling.CloseQuiet(f)
	}

	return execCmd, pipes, nil
}

// Attach to a container over HTTP
func attachExecHTTP(c *Container, sessionID string, r *http.Request, w http.ResponseWriter, streams *HTTPAttachStreams, pipes *execPipes, detachKeys []byte, isTerminal bool, cancel <-chan bool, hijackDone chan<- bool, holdConnOpen <-chan bool) (deferredErr error) {
	if pipes == nil || pipes.startPipe == nil || pipes.attachPipe == nil {
		return errors.Wrapf(define.ErrInvalidArg, "must provide a start and attach pipe to finish an exec attach")
	}

	defer func() {
		if !pipes.startClosed {
			errorhandling.CloseQuiet(pipes.startPipe)
			pipes.startClosed = true
		}
		if !pipes.attachClosed {
			errorhandling.CloseQuiet(pipes.attachPipe)
			pipes.attachClosed = true
		}
	}()

	logrus.Debugf("Attaching to container %s exec session %s", c.ID(), sessionID)

	// set up the socket path, such that it is the correct length and location for exec
	sockPath, err := c.execAttachSocketPath(sessionID)
	if err != nil {
		return err
	}
	socketPath := buildSocketPath(sockPath)

	// 2: read from attachFd that the parent process has set up the console socket
	if _, err := readConmonPipeData(pipes.attachPipe, ""); err != nil {
		return err
	}

	// 2: then attach
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: socketPath, Net: "unixpacket"})
	if err != nil {
		return errors.Wrapf(err, "failed to connect to container's attach socket: %v", socketPath)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			logrus.Errorf("unable to close socket: %q", err)
		}
	}()

	attachStdout := true
	attachStderr := true
	attachStdin := true
	if streams != nil {
		attachStdout = streams.Stdout
		attachStderr = streams.Stderr
		attachStdin = streams.Stdin
	}

	// Perform hijack
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return errors.Errorf("unable to hijack connection")
	}

	httpCon, httpBuf, err := hijacker.Hijack()
	if err != nil {
		return errors.Wrapf(err, "error hijacking connection")
	}

	hijackDone <- true

	// Write a header to let the client know what happened
	writeHijackHeader(r, httpBuf)

	// Force a flush after the header is written.
	if err := httpBuf.Flush(); err != nil {
		return errors.Wrapf(err, "error flushing HTTP hijack header")
	}

	go func() {
		// We need to hold the connection open until the complete exec
		// function has finished. This channel will be closed in a defer
		// in that function, so we can wait for it here.
		// Can't be a defer, because this would block the function from
		// returning.
		<-holdConnOpen
		hijackWriteErrorAndClose(deferredErr, c.ID(), isTerminal, httpCon, httpBuf)
	}()

	stdoutChan := make(chan error)
	stdinChan := make(chan error)

	// Next, STDIN. Avoid entirely if attachStdin unset.
	if attachStdin {
		go func() {
			logrus.Debugf("Beginning STDIN copy")
			_, err := utils.CopyDetachable(conn, httpBuf, detachKeys)
			logrus.Debugf("STDIN copy completed")
			stdinChan <- err
		}()
	}

	// 4: send start message to child
	if err := writeConmonPipeData(pipes.startPipe); err != nil {
		return err
	}

	// Handle STDOUT/STDERR *after* start message is sent
	go func() {
		var err error
		if isTerminal {
			// Hack: return immediately if attachStdout not set to
			// emulate Docker.
			// Basically, when terminal is set, STDERR goes nowhere.
			// Everything does over STDOUT.
			// Therefore, if not attaching STDOUT - we'll never copy
			// anything from here.
			logrus.Debugf("Performing terminal HTTP attach for container %s", c.ID())
			if attachStdout {
				err = httpAttachTerminalCopy(conn, httpBuf, c.ID())
			}
		} else {
			logrus.Debugf("Performing non-terminal HTTP attach for container %s", c.ID())
			err = httpAttachNonTerminalCopy(conn, httpBuf, c.ID(), attachStdin, attachStdout, attachStderr)
		}
		stdoutChan <- err
		logrus.Debugf("STDOUT/ERR copy completed")
	}()

	for {
		select {
		case err := <-stdoutChan:
			if err != nil {
				return err
			}

			return nil
		case err := <-stdinChan:
			if err != nil {
				return err
			}
		case <-cancel:
			return nil
		}
	}
}

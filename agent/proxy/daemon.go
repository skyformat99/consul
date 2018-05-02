package proxy

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"time"
)

// Constants related to restart timers with the daemon mode proxies. At some
// point we will probably want to expose these knobs to an end user, but
// reasonable defaults are chosen.
const (
	DaemonRestartHealthy    = 10 * time.Second // time before considering healthy
	DaemonRestartBackoffMin = 3                // 3 attempts before backing off
	DaemonRestartMaxWait    = 1 * time.Minute  // maximum backoff wait time
)

// Daemon is a long-running proxy process. It is expected to keep running
// and to use blocking queries to detect changes in configuration, certs,
// and more.
//
// Consul will ensure that if the daemon crashes, that it is restarted.
type Daemon struct {
	// Command is the command to execute to start this daemon. This must
	// be a Cmd that isn't yet started.
	Command *exec.Cmd

	// ProxyToken is the special local-only ACL token that allows a proxy
	// to communicate to the Connect-specific endpoints.
	ProxyToken string

	// Logger is where logs will be sent around the management of this
	// daemon. The actual logs for the daemon itself will be sent to
	// a file.
	Logger *log.Logger

	// For tests, they can set this to change the default duration to wait
	// for a graceful quit.
	gracefulWait time.Duration

	// process is the started process
	lock     sync.Mutex
	stopped  bool
	stopCh   chan struct{}
	exitedCh chan struct{}
	process  *os.Process
}

// Start starts the daemon and keeps it running.
//
// This function returns after the process is successfully started.
func (p *Daemon) Start() error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// A stopped proxy cannot be restarted
	if p.stopped {
		return fmt.Errorf("stopped")
	}

	// If we're already running, that is okay
	if p.process != nil {
		return nil
	}

	// Setup our stop channel
	stopCh := make(chan struct{})
	exitedCh := make(chan struct{})
	p.stopCh = stopCh
	p.exitedCh = exitedCh

	// Start the loop.
	go p.keepAlive(stopCh, exitedCh)

	return nil
}

// keepAlive starts and keeps the configured process alive until it
// is stopped via Stop.
func (p *Daemon) keepAlive(stopCh <-chan struct{}, exitedCh chan<- struct{}) {
	defer close(exitedCh)

	p.lock.Lock()
	process := p.process
	p.lock.Unlock()

	// attemptsDeadline is the time at which we consider the daemon to have
	// been alive long enough that we can reset the attempt counter.
	//
	// attempts keeps track of the number of restart attempts we've had and
	// is used to calculate the wait time using an exponential backoff.
	var attemptsDeadline time.Time
	var attempts uint

	for {
		if process == nil {
			// If we're passed the attempt deadline then reset the attempts
			if !attemptsDeadline.IsZero() && time.Now().After(attemptsDeadline) {
				attempts = 0
			}
			attemptsDeadline = time.Now().Add(DaemonRestartHealthy)
			attempts++

			// Calculate the exponential backoff and wait if we have to
			if attempts > DaemonRestartBackoffMin {
				waitTime := (1 << (attempts - DaemonRestartBackoffMin)) * time.Second
				if waitTime > DaemonRestartMaxWait {
					waitTime = DaemonRestartMaxWait
				}

				if waitTime > 0 {
					p.Logger.Printf(
						"[WARN] agent/proxy: waiting %s before restarting daemon",
						waitTime)

					timer := time.NewTimer(waitTime)
					select {
					case <-timer.C:
						// Timer is up, good!

					case <-stopCh:
						// During our backoff wait, we've been signalled to
						// quit, so just quit.
						timer.Stop()
						return
					}
				}
			}

			p.lock.Lock()

			// If we gracefully stopped then don't restart.
			if p.stopped {
				p.lock.Unlock()
				return
			}

			// Process isn't started currently. We're restarting. Start it
			// and save the process if we have it.
			var err error
			process, err = p.start()
			if err == nil {
				p.process = process
			}
			p.lock.Unlock()

			if err != nil {
				p.Logger.Printf("[ERR] agent/proxy: error restarting daemon: %s", err)
				continue
			}

		}

		ps, err := process.Wait()
		process = nil
		if err != nil {
			p.Logger.Printf("[INFO] agent/proxy: daemon exited with error: %s", err)
		} else if status, ok := exitStatus(ps); ok {
			p.Logger.Printf("[INFO] agent/proxy: daemon exited with exit code: %d", status)
		}
	}
}

// start starts and returns the process. This will create a copy of the
// configured *exec.Command with the modifications documented on Daemon
// such as setting the proxy token environmental variable.
func (p *Daemon) start() (*os.Process, error) {
	cmd := *p.Command

	// Add the proxy token to the environment. We first copy the env because
	// it is a slice and therefore the "copy" above will only copy the slice
	// reference. We allocate an exactly sized slice.
	cmd.Env = make([]string, len(p.Command.Env), len(p.Command.Env)+1)
	copy(cmd.Env, p.Command.Env)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", EnvProxyToken, p.ProxyToken))

	// TODO(mitchellh): temporary until we introduce the file based logging
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Args must always contain a 0 entry which is usually the executed binary.
	// To be safe and a bit more robust we default this, but only to prevent
	// a panic below.
	if len(cmd.Args) == 0 {
		cmd.Args = []string{cmd.Path}
	}

	// Start it
	p.Logger.Printf("[DEBUG] agent/proxy: starting proxy: %q %#v", cmd.Path, cmd.Args[1:])
	err := cmd.Start()
	return cmd.Process, err
}

// Stop stops the daemon.
//
// This will attempt a graceful stop (SIGINT) before force killing the
// process (SIGKILL). In either case, the process won't be automatically
// restarted unless Start is called again.
//
// This is safe to call multiple times. If the daemon is already stopped,
// then this returns no error.
func (p *Daemon) Stop() error {
	p.lock.Lock()

	// If we're already stopped or never started, then no problem.
	if p.stopped || p.process == nil {
		// In the case we never even started, calling Stop makes it so
		// that we can't ever start in the future, either, so mark this.
		p.stopped = true
		p.lock.Unlock()
		return nil
	}

	// Note that we've stopped
	p.stopped = true
	close(p.stopCh)
	process := p.process
	p.lock.Unlock()

	gracefulWait := p.gracefulWait
	if gracefulWait == 0 {
		gracefulWait = 5 * time.Second
	}

	// First, try a graceful stop
	err := process.Signal(os.Interrupt)
	if err == nil {
		select {
		case <-p.exitedCh:
			// Success!
			return nil

		case <-time.After(gracefulWait):
			// Interrupt didn't work
		}
	}

	// Graceful didn't work, forcibly kill
	return process.Kill()
}

// Equal implements Proxy to check for equality.
func (p *Daemon) Equal(raw Proxy) bool {
	p2, ok := raw.(*Daemon)
	if !ok {
		return false
	}

	// We compare equality on a subset of the command configuration
	return p.ProxyToken == p2.ProxyToken &&
		p.Command.Path == p2.Command.Path &&
		p.Command.Dir == p2.Command.Dir &&
		reflect.DeepEqual(p.Command.Args, p2.Command.Args) &&
		reflect.DeepEqual(p.Command.Env, p2.Command.Env)
}

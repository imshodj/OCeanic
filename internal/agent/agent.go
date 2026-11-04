package agent

import (
	"context"
	"errors"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/imshodj/oceanic/internal/protocol"
)

const inventoryEvery = 6 * time.Hour

// Agent is the long-running monitoring/management loop.
type Agent struct {
	cfg *Config
	c   *client

	mu            sync.Mutex
	needInventory bool
	jobSem        chan struct{}
}

// Run loads the installed config, enrolls if necessary and runs until ctx ends.
func Run(ctx context.Context) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	a := &Agent{cfg: cfg, c: newClient(cfg), needInventory: true, jobSem: make(chan struct{}, 4)}
	log.Printf("OCeanic agent %s starting (%s/%s), server %s", protocol.AgentVersion, runtime.GOOS, runtime.GOARCH, cfg.ServerURL)

	if !cfg.Enrolled() {
		if err := a.enrollUntilDone(ctx); err != nil {
			return err
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.checkinLoop(ctx) }()
	go func() { defer wg.Done(); a.jobLoop(ctx) }()
	wg.Wait()
	return nil
}

func (a *Agent) enrollUntilDone(ctx context.Context) error {
	backoff := 5 * time.Second
	for {
		err := Enroll(ctx, a.cfg)
		if err == nil {
			a.c = newClient(a.cfg)
			return nil
		}
		log.Printf("enroll failed: %v (retrying in %s)", err, backoff)
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

// Enroll exchanges the enrollment token for agent credentials and saves them.
func Enroll(ctx context.Context, cfg *Config) error {
	si := CollectSysInfo(false)
	c := newClient(&Config{ServerURL: cfg.ServerURL, CFAccessClientID: cfg.CFAccessClientID, CFAccessClientSecret: cfg.CFAccessClientSecret})
	resp, err := c.enroll(ctx, protocol.EnrollRequest{
		EnrollToken: cfg.EnrollToken,
		MachineID:   MachineID(),
		Hostname:    si.Hostname,
		SysInfo:     si,
	})
	if err != nil {
		return err
	}
	cfg.AgentID, cfg.AgentSecret = resp.AgentID, resp.AgentSecret
	cfg.EnrollToken = "" // no longer needed; don't keep it on disk
	if err := cfg.Save(); err != nil {
		return err
	}
	log.Printf("enrolled as agent %s", cfg.AgentID)
	return nil
}

func (a *Agent) checkinLoop(ctx context.Context) {
	interval := 30 * time.Second
	lastInv := time.Time{}
	backoff := 5 * time.Second
	for {
		req := protocol.CheckinRequest{AgentVersion: protocol.AgentVersion, Metrics: CollectMetrics()}
		a.mu.Lock()
		wantInv := a.needInventory || time.Since(lastInv) > inventoryEvery
		a.mu.Unlock()
		if wantInv {
			si := CollectSysInfo(true)
			req.SysInfo = &si
		}
		resp, err := a.c.checkin(ctx, req)
		wait := interval
		switch {
		case errors.Is(err, ErrGone):
			log.Printf("this device was deleted on the server; idling (reinstall to re-enroll)")
			wait = time.Hour
		case err != nil:
			log.Printf("checkin: %v", err)
			wait = backoff
			backoff = min(backoff*2, 2*time.Minute)
		default:
			backoff = 5 * time.Second
			if wantInv {
				lastInv = time.Now()
				a.mu.Lock()
				a.needInventory = false
				a.mu.Unlock()
			}
			if resp.Interval > 0 {
				interval = time.Duration(resp.Interval) * time.Second
				wait = interval
			}
			if resp.RefreshInventory {
				a.requestInventory()
			}
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

func (a *Agent) requestInventory() {
	a.mu.Lock()
	a.needInventory = true
	a.mu.Unlock()
}

// jobLoop long-polls the server; jobs are pushed almost instantly.
func (a *Agent) jobLoop(ctx context.Context) {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		jobs, err := a.c.jobs(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := backoff
			if errors.Is(err, ErrGone) {
				wait = time.Hour
			} else {
				log.Printf("jobs poll: %v", err)
				backoff = min(backoff*2, time.Minute)
			}
			sleepCtx(ctx, wait)
			continue
		}
		backoff = 2 * time.Second
		for _, j := range jobs {
			j := j
			if j.Type == protocol.JobTerminal {
				// Long-lived and interactive: don't occupy a script slot.
				go a.runTerminal(ctx, j)
				continue
			}
			if j.Type == protocol.JobDesktop {
				go a.runDesktop(ctx, j)
				continue
			}
			a.jobSem <- struct{}{}
			go func() {
				defer func() { <-a.jobSem }()
				a.handleJob(ctx, j)
			}()
		}
	}
}

func (a *Agent) report(ctx context.Context, r protocol.JobResult) {
	for i := 0; i < 5; i++ {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := a.c.result(rctx, r)
		cancel()
		if err == nil || errors.Is(err, ErrGone) {
			return
		}
		log.Printf("report result %s: %v", r.JobID, err)
		time.Sleep(time.Duration(i+1) * 3 * time.Second)
	}
}

func (a *Agent) handleJob(ctx context.Context, j protocol.Job) {
	log.Printf("job %s: %s %s", j.ID, j.Type, j.Shell)
	res := protocol.JobResult{JobID: j.ID, StartedAt: time.Now()}
	finish := func() {
		res.FinishedAt = time.Now()
		a.report(ctx, res)
	}
	switch j.Type {
	case protocol.JobScript:
		res.Output, res.ExitCode, res.Error = runScript(ctx, j)
		finish()
	case protocol.JobInventory:
		a.requestInventory()
		res.Output = "Inventory refresh scheduled for next check-in."
		finish()
	case protocol.JobReboot, protocol.JobShutdown:
		res.Output = "Issuing " + j.Type + " in 5 seconds."
		finish()
		if err := powerAction(j.Type == protocol.JobReboot); err != nil {
			log.Printf("power action: %v", err)
		}
	case protocol.JobUpdate:
		if err := a.selfUpdate(ctx); err != nil {
			res.ExitCode, res.Error = 1, err.Error()
			finish()
			return
		}
		res.Output = "Agent binary updated; restarting service."
		finish()
		restartSelf()
	case protocol.JobUninstall:
		res.Output = "Agent is uninstalling itself."
		finish()
		if err := scheduleUninstall(); err != nil {
			log.Printf("uninstall: %v", err)
		}
	default:
		res.ExitCode, res.Error = 1, "unsupported job type "+j.Type
		finish()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

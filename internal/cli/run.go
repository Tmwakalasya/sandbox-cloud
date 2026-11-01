package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"sandbox-cloud/internal/client"
	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

func runTask(ctx context.Context, c *client.Client, args []string, machine bool, out, errout io.Writer) (resultErr error) {
	f := flags("run", errout)
	ttl := f.Duration("ttl", 20*time.Minute, "workspace lifetime, including setup")
	wait := f.Duration("wait", 3*time.Minute, "maximum readiness wait")
	timeout := f.Duration("timeout", 60*time.Second, "command timeout")
	keep := f.Bool("keep", false, "retain workspace until TTL, including on failure or interruption")
	f.Usage = func() {
		fmt.Fprintln(errout, "Usage: sandbox run [OPTIONS] LOCAL_DIR -- COMMAND [ARGS...]")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	a := f.Args()
	if len(a) < 3 || a[1] != "--" {
		return errors.New("usage: sandbox run [OPTIONS] LOCAL_DIR -- COMMAND [ARGS...]")
	}
	if *ttl < time.Minute || *ttl > 24*time.Hour || *ttl%time.Second != 0 {
		return errors.New("ttl must be whole seconds between 1m and 24h")
	}
	if *wait <= 0 {
		return errors.New("wait must be positive")
	}
	if *timeout < time.Second || *timeout > time.Hour || *timeout%time.Second != 0 {
		return errors.New("timeout must be whole seconds between 1s and 1h")
	}
	info, err := os.Lstat(a[0])
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("LOCAL_DIR must be a directory (not a symlink)")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id := newID("sbx")
	fmt.Fprintln(errout, "Workspace ID:", id)
	// Install cleanup before create: an interrupted/lost response can still mean a Pod exists.
	defer func() {
		if *keep {
			fmt.Fprintf(errout, "Workspace retained (if created) until its TTL: %s\nInspect: sandbox --api %s inspect %s\n", id, c.Base, id)
			return
		}
		// Signal cancellation must not cancel the cleanup request as well.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.Destroy(cleanupCtx, id); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("cleanup %s: %w; TTL remains the fallback; retry with sandbox --api %s destroy %s", id, err, c.Base, id))
		} else {
			fmt.Fprintln(errout, "Deletion requested:", id)
		}
	}()
	if _, err := c.Create(ctx, cloud.CreateRequest{ID: id, TTLSeconds: int64(ttl.Seconds())}); err != nil {
		return fmt.Errorf("create %s: %w", id, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, *wait)
	defer cancel()
	var last string
	for {
		w, err := c.Get(waitCtx, id)
		if err != nil {
			return fmt.Errorf("wait for %s: %w", id, err)
		}
		if !machine && last != w.State+w.Reason {
			fmt.Fprintf(errout, "%s  %s\n", clean(w.State), clean(w.Reason))
			last = w.State + w.Reason
		}
		if w.State == "ready" {
			break
		}
		switch w.State {
		case "terminated", "expired", "terminating", "unavailable":
			return fmt.Errorf("workspace %s: %s %s", id, w.State, w.Reason)
		}
		if err := pause(waitCtx, time.Second); err != nil {
			return fmt.Errorf("wait for %s: %w", id, err)
		}
	}
	if _, err := upload(ctx, c, id, a[0], "."); err != nil {
		return err
	}
	commandID := newID("cmd")
	fmt.Fprintln(errout, "Command ID:", commandID)
	if _, err := c.Start(ctx, id, runner.Request{ID: commandID, Command: a[2:], TimeoutSeconds: int(timeout.Seconds())}); err != nil {
		return fmt.Errorf("start %s: %w", commandID, err)
	}
	return stream(ctx, c, id, commandID, machine, out, errout)
}

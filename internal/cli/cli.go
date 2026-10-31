package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"sandbox-cloud/internal/client"
	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

const usage = `sandbox — disposable remote workspaces

Usage: sandbox [--api URL] [--json] COMMAND [OPTIONS]

  create  [--ttl 20m] [--wait 3m]     Create and wait for a workspace
  list                              List workspace status
  inspect WORKSPACE                 Inspect one workspace
  watch [WORKSPACE]                 Live workspace dashboard
  put WORKSPACE LOCAL_PATH REMOTE    Upload a file or directory
  exec [--timeout 60s] WORKSPACE -- COMMAND [ARGS...]
                                    Execute and stream output
  logs WORKSPACE COMMAND_ID         Replay output and follow execution
  result WORKSPACE COMMAND_ID       Inspect a command result
  cancel WORKSPACE COMMAND_ID       Cancel a command
  destroy WORKSPACE                 Request workspace deletion
  preview                           Preview the animated UI (no network)

Global flags go before the command; command flags go before positional args.
API defaults to SANDBOX_API or http://127.0.0.1:8080 via kubectl port-forward.
Closing the CLI does not cancel commands or destroy workspaces.
`

type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }
func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%x", prefix, b)
}
func flags(name string, out io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(out)
	return f
}
func tty(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd()) && os.Getenv("TERM") != "dumb"
}
func stdinTTY() bool                     { return term.IsTerminal(os.Stdin.Fd()) }
func jsonOut(out io.Writer, v any) error { return json.NewEncoder(out).Encode(v) }
func table(out io.Writer, items []cloud.Workspace) error {
	tw := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
	fmt.Fprintln(tw, "WORKSPACE\tSTATE\tREMAINING\tCPU\tMEMORY")
	for _, w := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", clean(w.ID), clean(w.State), remaining(w.ExpiresAt), w.CPU, w.Memory)
	}
	return tw.Flush()
}
func Run(ctx context.Context, args []string, out, errout io.Writer) error {
	global := flags("sandbox", errout)
	baseDefault := os.Getenv("SANDBOX_API")
	if baseDefault == "" {
		baseDefault = "http://127.0.0.1:8080"
	}
	base := global.String("api", baseDefault, "control API URL")
	machine := global.Bool("json", false, "emit JSON without animations")
	global.Usage = func() { fmt.Fprint(out, usage) }
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = global.Args()
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(out, usage)
		return nil
	}
	if args[0] == "preview" {
		if len(args) != 1 {
			return errors.New("preview takes no arguments")
		}
		started := time.Now()
		fetch := func(context.Context) ([]cloud.Workspace, error) {
			w := cloud.Workspace{ID: "sbx-0123456789abcdef01234567", State: "pending", Reason: "Waiting for a worker", CreatedAt: started, ExpiresAt: started.Add(20 * time.Minute), CPU: "1", Memory: "512Mi"}
			if time.Since(started) > time.Second {
				w.State = "starting"
				w.Node = "gke-demo-worker"
				w.Reason = "ContainerCreating"
			}
			if time.Since(started) > 3*time.Second {
				w.State = "ready"
				w.Reason = ""
			}
			return []cloud.Workspace{w}, nil
		}
		if *machine {
			w, _ := fetch(ctx)
			return jsonOut(out, map[string]any{"simulated": true, "workspaces": w})
		}
		if !tty(out) || !stdinTTY() {
			fmt.Fprintln(out, "UI PREVIEW (simulated; use an interactive terminal for animation)")
			w, _ := fetch(ctx)
			return table(out, w)
		}
		return show(ctx, out, fetch, true, true)
	}
	c, err := client.New(*base)
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		f := flags("create", errout)
		ttl := f.Duration("ttl", 20*time.Minute, "workspace lifetime")
		wait := f.Duration("wait", 3*time.Minute, "readiness wait; zero returns immediately")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("create takes no positional arguments")
		}
		if *ttl < time.Minute || *ttl > 24*time.Hour || *ttl%time.Second != 0 {
			return errors.New("ttl must be whole seconds between 1m and 24h")
		}
		if *wait < 0 {
			return errors.New("wait cannot be negative")
		}
		id := newID("sbx")
		fmt.Fprintln(errout, "Workspace ID:", id)
		w, err := c.Create(ctx, cloud.CreateRequest{ID: id, TTLSeconds: int64(ttl.Seconds())})
		if err != nil {
			return fmt.Errorf("create %s: %w; inspect this ID before submitting another request", id, err)
		}
		if *wait == 0 {
			if *machine {
				return jsonOut(out, w)
			}
			return table(out, []cloud.Workspace{w})
		}
		waitCtx, cancel := context.WithTimeout(ctx, *wait)
		defer cancel()
		fetch := func(ctx context.Context) ([]cloud.Workspace, error) {
			w, err := c.Get(ctx, id)
			return []cloud.Workspace{w}, err
		}
		if !*machine && tty(out) && stdinTTY() {
			return show(waitCtx, out, fetch, true, false)
		}
		var last string
		for {
			w, err = c.Get(waitCtx, id)
			if err != nil {
				return fmt.Errorf("wait for %s: %w; workspace remains until its TTL", id, err)
			}
			if !*machine && w.State+w.Reason != last {
				fmt.Fprintf(errout, "%s  %s\n", clean(w.State), clean(w.Reason))
				last = w.State + w.Reason
			}
			switch w.State {
			case "ready":
				if *machine {
					return jsonOut(out, w)
				}
				return table(out, []cloud.Workspace{w})
			case "terminated", "expired", "terminating", "unavailable":
				return fmt.Errorf("workspace %s: %s %s", id, w.State, w.Reason)
			}
			if err := pause(waitCtx, time.Second); err != nil {
				return fmt.Errorf("wait for %s: %w; workspace remains until its TTL", id, err)
			}
		}
	case "list", "inspect", "watch":
		command := args[0]
		if command == "list" && len(args) != 1 || command == "inspect" && len(args) != 2 || command == "watch" && len(args) > 2 {
			return fmt.Errorf("invalid arguments for %s", command)
		}
		fetch := func(ctx context.Context) ([]cloud.Workspace, error) {
			if len(args) == 2 {
				w, err := c.Get(ctx, args[1])
				return []cloud.Workspace{w}, err
			}
			return c.List(ctx)
		}
		if command == "watch" && !*machine && tty(out) && stdinTTY() {
			return show(ctx, out, fetch, false, false)
		}
		for {
			items, err := fetch(ctx)
			if err != nil {
				return err
			}
			if *machine {
				if command == "inspect" {
					err = jsonOut(out, items[0])
				} else {
					err = jsonOut(out, items)
				}
			} else {
				err = table(out, items)
				if command == "inspect" && items[0].Reason != "" {
					fmt.Fprintln(out, clean(items[0].Reason))
				}
			}
			if err != nil {
				return err
			}
			if command != "watch" {
				return nil
			}
			if err := pause(ctx, time.Second); err != nil {
				return err
			}
		}
	case "destroy":
		if len(args) != 2 {
			return errors.New("usage: sandbox destroy WORKSPACE")
		}
		if err := c.Destroy(ctx, args[1]); err != nil {
			return err
		}
		if *machine {
			return jsonOut(out, map[string]string{"id": args[1], "state": "deletion_requested"})
		}
		fmt.Fprintln(out, "Deletion requested:", args[1])
		return nil
	case "put":
		if len(args) != 4 {
			return errors.New("usage: sandbox put WORKSPACE LOCAL_PATH REMOTE_RELATIVE_PATH")
		}
		count, err := upload(ctx, c, args[1], args[2], args[3])
		if err != nil {
			return err
		}
		if *machine {
			return jsonOut(out, map[string]int{"files_uploaded": count})
		}
		fmt.Fprintf(out, "Uploaded %d file(s) to %s\n", count, clean(args[1]))
		return nil
	case "exec":
		f := flags("exec", errout)
		timeout := f.Duration("timeout", 60*time.Second, "command timeout")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		a := f.Args()
		if len(a) < 2 {
			return errors.New("usage: sandbox exec [--timeout 60s] WORKSPACE -- COMMAND [ARGS...]")
		}
		id := a[0]
		command := a[1:]
		if command[0] == "--" {
			command = command[1:]
		}
		if len(command) == 0 {
			return errors.New("missing command")
		}
		if *timeout < time.Second || *timeout > time.Hour || *timeout%time.Second != 0 {
			return errors.New("timeout must be whole seconds between 1s and 1h")
		}
		commandID := newID("cmd")
		fmt.Fprintln(errout, "Command ID:", commandID)
		_, err := c.Start(ctx, id, runner.Request{ID: commandID, Command: command, TimeoutSeconds: int(timeout.Seconds())})
		if err != nil {
			return fmt.Errorf("start %s: %w; inspect with sandbox result %s %s before retrying", commandID, err, id, commandID)
		}
		return stream(ctx, c, id, commandID, *machine, out, errout)
	case "logs", "result", "cancel":
		if len(args) != 3 {
			return fmt.Errorf("usage: sandbox %s WORKSPACE COMMAND_ID", args[0])
		}
		switch args[0] {
		case "logs":
			return stream(ctx, c, args[1], args[2], *machine, out, errout)
		case "cancel":
			if err := c.Cancel(ctx, args[1], args[2]); err != nil {
				return err
			}
			if *machine {
				return jsonOut(out, map[string]string{"state": "cancellation_requested"})
			}
			fmt.Fprintln(out, "Cancellation requested:", args[2])
			return nil
		default:
			r, err := c.Command(ctx, args[1], args[2])
			if err != nil {
				return err
			}
			return jsonOut(out, r)
		}
	default:
		return fmt.Errorf("unknown command %q; run sandbox help", args[0])
	}
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func stream(ctx context.Context, c *client.Client, id, command string, machine bool, out, errout io.Writer) error {
	r, err := c.Events(ctx, id, command, func(e runner.Event) error {
		if machine {
			return jsonOut(out, e)
		}
		if e.Stream == "stdout" || e.Stream == "stderr" {
			target := out
			if e.Stream == "stderr" {
				target = errout
			}
			data := e.Data
			if tty(target) {
				data = ansi.Strip(data)
			}
			_, err := io.WriteString(target, data)
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w; reconnect with sandbox logs %s %s", err, id, command)
	}
	if !machine {
		elapsed := time.Since(r.StartedAt)
		if r.FinishedAt != nil {
			elapsed = r.FinishedAt.Sub(r.StartedAt)
		}
		fmt.Fprintf(errout, "\n%s · %s", strings.ToUpper(r.State), elapsed.Round(time.Millisecond))
		if r.ExitCode != nil {
			fmt.Fprintf(errout, " · exit %d", *r.ExitCode)
		}
		if r.Truncated {
			fmt.Fprint(errout, " · output truncated")
		}
		fmt.Fprintln(errout)
	}
	if r.State != "succeeded" {
		code := 1
		if r.ExitCode != nil && *r.ExitCode > 0 && *r.ExitCode < 126 {
			code = *r.ExitCode
		}
		return &ExitError{code, "command " + r.State + ": " + r.Error}
	}
	return nil
}
func upload(ctx context.Context, c *client.Client, id, local, remote string) (int, error) {
	if !filepath.IsLocal(remote) {
		return 0, errors.New("remote path must be relative to /workspace")
	}
	info, err := os.Lstat(local)
	if err != nil {
		return 0, err
	}
	count := 0
	send := func(path, dest string, info fs.FileInfo) error {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: only regular files are supported (no symlinks)", path)
		}
		if info.Size() > runner.MaxUpload {
			return fmt.Errorf("%s exceeds 8 MiB upload limit", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := c.Put(ctx, id, filepath.ToSlash(dest), f); err != nil {
			return fmt.Errorf("upload %s: %w", path, err)
		}
		count++
		return nil
	}
	if !info.IsDir() {
		err := send(local, remote, info)
		return count, err
	}
	err = filepath.WalkDir(local, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(local, path)
		if err != nil {
			return err
		}
		return send(path, filepath.Join(remote, rel), info)
	})
	return count, err
}

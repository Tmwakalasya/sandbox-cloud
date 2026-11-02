package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"sandbox-cloud/internal/client"
	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

var commitSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// Values travel as positional arguments, never interpolated shell source. Each
// run owns a fresh workspace; setup failure prevents the user's command running.
// Git receives no interactive credentials, hooks, submodules, or local config.
const repositoryScript = `set -eu
repo=$1
revision=$2
shift 2
export GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_LFS_SKIP_SMUDGE=1
git init -q --template= .sandbox-repo
cd .sandbox-repo
git -c credential.helper= -c protocol.allow=never -c protocol.https.allow=always -c http.followRedirects=false fetch --quiet --depth=1 --no-tags -- "$repo" "$revision"
git -c core.hooksPath=/dev/null checkout --quiet --detach FETCH_HEAD
actual=$(git rev-parse HEAD)
test "$actual" = "$revision"
printf 'Checked out %s\n' "$actual" >&2
exec "$@"
`

func runRepository(ctx context.Context, c *client.Client, args []string, machine bool, out, errout io.Writer) (runErr error) {
	f := flags("run", errout)
	repo := f.String("repo", "", "public HTTPS repository URL; no embedded credentials")
	ref := f.String("ref", "", "full 40-character commit SHA")
	ttl := f.Duration("ttl", 20*time.Minute, "workspace expiry backstop")
	wait := f.Duration("wait", 3*time.Minute, "workspace readiness deadline")
	timeout := f.Duration("timeout", 5*time.Minute, "combined Git setup and command deadline")
	keep := f.Bool("keep-on-failure", false, "retain failed/interrupted workspace until TTL")
	if err := f.Parse(args); err != nil {
		return err
	}
	command := f.Args()
	if len(command) == 0 || command[0] == "" {
		return errors.New("usage: sandbox run --repo HTTPS_URL --ref COMMIT [OPTIONS] -- COMMAND [ARGS...]")
	}
	if len(command) > 122 {
		return errors.New("run supports at most 122 command arguments including the executable")
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return errors.New("command contains NUL")
		}
	}
	u, err := url.Parse(*repo)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path == "" || u.Path == "/" || strings.ContainsAny(*repo, "\x00\r\n\t") {
		return errors.New("repo must be an HTTPS repository URL without credentials, query, or fragment")
	}
	if !commitSHA.MatchString(*ref) {
		return errors.New("ref must be a full 40-character commit SHA, not a branch or tag")
	}
	if *wait <= 0 || *timeout < time.Second || *timeout > time.Hour || *timeout%time.Second != 0 || *ttl < time.Minute || *ttl > 24*time.Hour || *ttl%time.Second != 0 || *wait > *ttl || *timeout > *ttl-*wait || *ttl-*wait-*timeout < time.Minute {
		return errors.New("wait must be positive; timeout whole seconds from 1s to 1h; TTL whole seconds from 1m to 24h covering wait + timeout + 1m")
	}
	sha := strings.ToLower(*ref)
	id := newID("sbx")
	stage := func(name, message string) error {
		if machine {
			return jsonOut(out, map[string]string{"stage": name, "workspace_id": id, "message": message})
		}
		_, err := fmt.Fprintf(errout, "[%s] %s\n", name, message)
		return err
	}
	if err := stage("allocating", "Workspace ID: "+id); err != nil {
		return err
	}
	// Install cleanup before create: the server may allocate successfully even
	// if its response is lost. No new workspace ID or automatic retry is issued.
	defer func() {
		if runErr != nil && *keep {
			if err := stage("retained", "Workspace retained until TTL; inspect with sandbox inspect "+id); err != nil {
				runErr = errors.Join(runErr, err)
			}
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Destroy(cleanupCtx, id); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("cleanup %s: %w; inspect this workspace, TTL remains the backstop", id, err))
			return
		}
		runErr = errors.Join(runErr, stage("cleanup", "Deletion requested: "+id))
	}()
	readyCtx, cancel := context.WithTimeout(ctx, *wait)
	defer cancel()
	w, err := c.Create(readyCtx, cloud.CreateRequest{ID: id, TTLSeconds: int64(ttl.Seconds())})
	for err == nil && w.State != "ready" {
		if w.State != "pending" && w.State != "starting" {
			return fmt.Errorf("workspace %s: %s %s", id, w.State, w.Reason)
		}
		if err = pause(readyCtx, time.Second); err != nil {
			break
		}
		w, err = c.Get(readyCtx, id)
	}
	if err != nil {
		return fmt.Errorf("allocate %s: %w", id, err)
	}
	commandID := newID("cmd")
	if err := stage("executing", "Command ID: "+commandID+"; preparing "+sha+" and running command"); err != nil {
		return err
	}
	remote := append([]string{"sh", "-c", repositoryScript, "sandbox-run", *repo, sha}, command...)
	// The runner enforces the execution deadline; bound HTTP/event waiting too.
	execCtx, execCancel := context.WithTimeout(ctx, *timeout+30*time.Second)
	defer execCancel()
	if _, err := c.Start(execCtx, id, runner.Request{ID: commandID, Command: remote, TimeoutSeconds: int(timeout.Seconds())}); err != nil {
		return fmt.Errorf("start %s in %s: %w", commandID, id, err)
	}
	// A cleanup-bound run must not suggest reconnecting to a deleted workspace.
	return streamCommand(execCtx, c, id, commandID, machine, *keep, out, errout)
}

# Run from the repository root after make build and port-forwarding.
# Export KUBECONFIG, SANDBOX_CONTEXT, and SANDBOX_API for the same deployment.
# Requires capacity for ten active workspaces and two ready idle Pods.
Output demo-burst.mp4
Require zsh
Require kubectl

Set Shell "zsh"
Set FontSize 17
Set Width 1440
Set Height 1080
Set Padding 20
Set TypingSpeed 30ms
Set Theme "GitHub Dark"
Set CursorBlink false
Set Framerate 20

Env TERM "xterm-256color"
Env COLORTERM "truecolor"
Env CLICOLOR_FORCE "1"
Env NO_COLOR ""

Hide
Type@0ms `PROMPT='sandbox > '; RPROMPT=; setopt ERR_EXIT; unsetopt MONITOR; export PATH="$PWD/bin:$PATH"; : "${KUBECONFIG:?export KUBECONFIG}" "${SANDBOX_CONTEXT:?export SANDBOX_CONTEXT}" "${SANDBOX_API:?export SANDBOX_API}"; mkdir -p .local; BENCH_DIR=$(mktemp -d "$PWD/.local/demo-burst.XXXXXX"); clear`
Enter
Wait+Line@10s /^sandbox > ?$/
Type@0ms `burst10() { sandbox-bench --kubeconfig "$KUBECONFIG" --context "$SANDBOX_CONTEXT" --api "$SANDBOX_API" --count 10 --rounds 1 --idle 2 --hold 25s --output "$BENCH_DIR/results.json" > "$BENCH_DIR/progress.log" 2>&1 & BENCH_PID=$!; }; pool() { kubectl --context "$SANDBOX_CONTEXT" -n sandbox-workspaces get pods -l 'sandbox-cloud/pool=true,!sandbox-cloud/claim-uid'; }; clear`
Enter
Wait+Line@10s /^sandbox > ?$/
Show

Type `pool`
Enter
Wait+Line@30s /^sandbox > ?$/
Sleep 2s
Type `burst10`
Enter
Wait+Line@10s /^sandbox > ?$/
Type `sandbox watch || test "$?" -eq 130`
Enter
Wait+Screen@6m /(?s)(READY.*){10}/
Sleep 7s
Type `q`
Wait+Line@10s /^sandbox > ?$/
Ctrl+L
Type `wait "$BENCH_PID"; cat "$BENCH_DIR/progress.log"`
Enter
Wait+Line@3m /^sandbox > ?$/
Sleep 6s

Type `sandbox list`
Enter
Wait+Line@30s /^sandbox > ?$/
Sleep 2s
Type `pool`
Enter
Wait+Line@30s /^sandbox > ?$/
Sleep 4s

# Run from the repository root after deployment and port-forwarding.
# Outputs preserve the original demo.mp4 and demo.gif.
Output demo-v2.mp4
Output demo-v2.gif
Require zsh
Require sed

Set Shell "zsh"
Set FontSize 18
Set Width 1280
Set Height 720
Set Padding 24
Set TypingSpeed 30ms
Set Theme "GitHub Dark"
Set CursorBlink false

Env TERM "xterm-256color"

# Keep stdout attached to the terminal so create retains its animated UI.
# Capture create stderr to identify only this recording's workspace.
# Restore stderr before execution so streamed output stays in order.
Hide
Type@0ms `PROMPT='sandbox > '; RPROMPT=; SANDBOX_DEMO_LOG=$(mktemp); exec 3>&2; exec 2> "$SANDBOX_DEMO_LOG"; setopt ERR_EXIT; clear`
Enter
Wait+Line@10s /^sandbox > ?$/
Show
Sleep 1s

Type `./bin/sandbox create --ttl 10m --wait 5m`
Enter
Wait+Line@6m /^sandbox > ?$/
Sleep 2s

Hide
Type@0ms `ID=$(sed -n 's/.*Workspace ID: \(sbx-[0-9a-f]\{24\}\).*/\1/p' "$SANDBOX_DEMO_LOG" | head -n 1); exec 2>&3 3>&-; [[ -n "$ID" ]]`
Enter
Wait+Line@10s /^sandbox > ?$/
Ctrl+L
Sleep 300ms
Show

Type `./bin/sandbox put "$ID" ./examples/hello .`
Enter
Wait+Line@45s /^sandbox > ?$/
Sleep 1s

Type `./bin/sandbox exec --timeout 180s "$ID" -- go test -v ./...`
Enter
Wait+Line@4m /^sandbox > ?$/
Sleep 3s

Type `./bin/sandbox exec --timeout 180s "$ID" -- go run .`
Enter
Wait+Line@4m /^sandbox > ?$/
Sleep 2s

Type `./bin/sandbox list`
Enter
Wait+Line@45s /^sandbox > ?$/
Sleep 2s

Type `./bin/sandbox destroy "$ID"`
Enter
Wait+Line@45s /^sandbox > ?$/
Sleep 3s

Hide
Type@0ms `rm -f -- "$SANDBOX_DEMO_LOG"; clear`
Enter
Wait+Line@10s /^sandbox > ?$/

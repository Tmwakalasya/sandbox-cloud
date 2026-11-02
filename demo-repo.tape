# Run from the repository root after make build and API port-forwarding.
# Fetches a public repository and Go dependencies inside the remote sandbox.
# This focused Go package run is not SubmitQueue's full Bazel CI suite.
# The commit is pinned from the locally cached uber/submitqueue origin/main.
Output demo-repo.mp4
Require zsh

Set Shell "zsh"
Set FontSize 19
Set Width 1440
Set Height 900
Set Padding 24
Set TypingSpeed 25ms
Set Theme "GitHub Dark"
Set CursorBlink false
Set Framerate 20

Env TERM "xterm-256color"
Env COLORTERM "truecolor"
Env CLICOLOR_FORCE "1"
Env NO_COLOR ""

Hide
Type@0ms `PROMPT='sandbox > '; RPROMPT=; setopt ERR_EXIT; export PATH="$PWD/bin:$PATH"; clear`
Enter
Wait+Line@10s /^sandbox > ?$/
Show

Type `REPO=https://github.com/uber/submitqueue.git`
Enter
Wait+Line@10s /^sandbox > ?$/
Type `COMMIT=52e3b0bae90e035480dc41e5b429c4cdd57d8ec2`
Enter
Wait+Line@10s /^sandbox > ?$/
Sleep 1s
Type `sandbox run --repo "$REPO" --ref "$COMMIT" --timeout 8m -- env GOMAXPROCS=2 GOTOOLCHAIN=local go test -mod=readonly -p 1 -count=1 ./platform/base/change/git`
Enter
Wait+Line@12m /^sandbox > ?$/
Sleep 5s
Type `sandbox list`
Enter
Wait+Line@30s /^sandbox > ?$/
Sleep 4s

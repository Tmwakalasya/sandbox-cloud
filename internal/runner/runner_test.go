package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		h.Close()
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, h.URL, dir
}

func call(t *testing.T, method, address string, body any, status int) []byte {
	t.Helper()
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, address, input)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, address, resp.StatusCode, status, b)
	}
	return b
}

func submit(t *testing.T, base string, r Request) Result {
	t.Helper()
	var result Result
	if err := json.Unmarshal(call(t, "POST", base+"/commands", r, 202), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func collect(t *testing.T, base, id string) ([]Event, Result) {
	t.Helper()
	b := call(t, "GET", base+"/commands/"+id+"/events", nil, 200)
	d := json.NewDecoder(bytes.NewReader(b))
	var events []Event
	for {
		var e Event
		err := d.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
		if e.Result != nil {
			return events, *e.Result
		}
	}
	t.Fatal("stream ended without result")
	return nil, Result{}
}

func TestUploadExecuteAndRetainFiles(t *testing.T) {
	_, base, _ := setup(t)
	req, err := http.NewRequest("PUT", base+"/files?path=src/message.txt", strings.NewReader("hello workspace"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
	r := submit(t, base, Request{Command: []string{"sh", "-c", "cat src/message.txt; printf error >&2; printf saved > result.txt"}})
	events, result := collect(t, base, r.ID)
	if result.State != "succeeded" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("result: %+v", result)
	}
	var stdout, stderr string
	for _, e := range events {
		if e.Stream == "stdout" {
			stdout += e.Data
		}
		if e.Stream == "stderr" {
			stderr += e.Data
		}
	}
	if stdout != "hello workspace" || stderr != "error" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	r = submit(t, base, Request{Command: []string{"cat", "result.txt"}})
	events, result = collect(t, base, r.ID)
	if result.State != "succeeded" || events[0].Data != "saved" {
		t.Fatalf("files not retained: %+v", events)
	}
}

func TestDeadlineBusyAndIdempotency(t *testing.T) {
	_, base, _ := setup(t)
	req := Request{ID: "cmd-0123456789abcdef01234567", Command: []string{"sh", "-c", "sleep 30"}, TimeoutSeconds: 1}
	r := submit(t, base, req)
	call(t, "POST", base+"/commands", req, 200)
	call(t, "POST", base+"/commands", Request{Command: []string{"true"}}, 409)
	req.Command = []string{"false"}
	call(t, "POST", base+"/commands", req, 409)
	_, result := collect(t, base, r.ID)
	if result.State != "timed_out" {
		t.Fatalf("result: %+v", result)
	}
	r = submit(t, base, Request{Command: []string{"sh", "-c", "exit 7"}})
	_, result = collect(t, base, r.ID)
	if result.State != "failed" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result: %+v", result)
	}
}

func TestCancelAndDisconnect(t *testing.T) {
	_, base, _ := setup(t)
	r := submit(t, base, Request{Command: []string{"sh", "-c", "printf started; sleep 30"}})
	resp, err := http.Get(base + "/commands/" + r.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var current Result
	if err := json.Unmarshal(call(t, "GET", base+"/commands/"+r.ID, nil, 200), &current); err != nil {
		t.Fatal(err)
	}
	if current.State != "running" {
		t.Fatalf("disconnect cancelled command: %+v", current)
	}
	call(t, "POST", base+"/commands/"+r.ID+"/cancel", nil, 202)
	_, result := collect(t, base, r.ID)
	if result.State != "cancelled" {
		t.Fatalf("result: %+v", result)
	}
}

func TestUploadRejectsEscapeAndOversize(t *testing.T) {
	_, base, dir := setup(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside.txt", "/absolute.txt", "escape/outside.txt"} {
		req, err := http.NewRequest("PUT", base+"/files?path="+url.QueryEscape(name), strings.NewReader("bad"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%s: status %d", name, resp.StatusCode)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("PUT", base+"/files?path=keep.txt", strings.NewReader(strings.Repeat("x", MaxUpload+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("oversize: %d", resp.StatusCode)
	}
	b, err := os.ReadFile(filepath.Join(dir, "keep.txt"))
	if err != nil || string(b) != "original" {
		t.Fatalf("existing file changed: %q %v", b, err)
	}
}

func TestOutputBoundAndMissingExecutable(t *testing.T) {
	s, base, _ := setup(t)
	call(t, "POST", base+"/commands", Request{Command: []string{"/no/such/executable"}}, 422)
	r := submit(t, base, Request{Command: []string{"sh", "-c", fmt.Sprintf("head -c %d /dev/zero", MaxOutput+100)}})
	_, result := collect(t, base, r.ID)
	if !result.Truncated || result.State != "succeeded" {
		t.Fatalf("result: %+v", result)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commands[r.ID].bytes > MaxOutput {
		t.Fatal("output bound exceeded")
	}
}

func TestConcurrentSubscribersReceiveCompleteOutput(t *testing.T) {
	_, base, _ := setup(t)
	r := submit(t, base, Request{Command: []string{"sh", "-c", "i=0; while [ $i -lt 20 ]; do printf 'chunk-'; sleep 0.01; i=$((i+1)); done"}})
	type captured struct {
		output string
		result *Result
		err    error
	}
	readers := make(chan captured, 2)
	for range 2 {
		go func() {
			response, err := http.Get(base + "/commands/" + r.ID + "/events")
			if err != nil {
				readers <- captured{err: err}
				return
			}
			defer response.Body.Close()
			var got captured
			decoder := json.NewDecoder(response.Body)
			for {
				var event Event
				err := decoder.Decode(&event)
				if err == io.EOF {
					break
				}
				if err != nil {
					got.err = err
					break
				}
				if event.Stream == "stdout" {
					got.output += event.Data
				}
				if event.Result != nil {
					got.result = event.Result
				}
			}
			readers <- got
		}()
	}
	for range 2 {
		select {
		case got := <-readers:
			if got.err != nil || got.output != strings.Repeat("chunk-", 20) || got.result == nil || got.result.State != "succeeded" {
				t.Fatalf("subscriber: %+v", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("subscriber missed completion notification")
		}
	}
}

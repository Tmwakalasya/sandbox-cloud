package main

import "testing"

func TestGreeting(t *testing.T) {
	if got := Greeting("sandbox"); got != "hello, sandbox" {
		t.Fatalf("Greeting: got %q", got)
	}
}

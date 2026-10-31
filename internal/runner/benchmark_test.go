package runner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkCommandRoundTrip measures local HTTP submission, spawning `true`,
// and following its result. It does not measure container or GKE startup.
func BenchmarkCommandRoundTrip(b *testing.B) {
	r, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	client := server.Client()
	body := []byte(`{"command":["true"]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		response, err := client.Post(server.URL+"/commands", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		var result Result
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusAccepted {
			b.Fatalf("submit: %v status %d", err, response.StatusCode)
		}
		response, err = client.Get(server.URL + "/commands/" + result.ID + "/events")
		if err != nil {
			b.Fatal(err)
		}
		decoder := json.NewDecoder(response.Body)
		finished := false
		for {
			var e Event
			err := decoder.Decode(&e)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			if e.Result != nil {
				if e.Result.State != "succeeded" {
					b.Fatalf("result: %+v", e.Result)
				}
				finished = true
			}
		}
		response.Body.Close()
		if !finished {
			b.Fatal("no final result")
		}
	}
}

package queue

import (
	"context"
	"net"
	"testing"
)

// fakeNATS accepts connections and writes banner, like a real server does.
func fakeNATS(t *testing.T, banner string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(banner))
			c.Close()
		}
	}()
	return "nats://" + ln.Addr().String()
}

func TestPing(t *testing.T) {
	js := fakeNATS(t, "INFO {\"server_id\":\"x\",\"jetstream\":true}\r\n")
	noJS := fakeNATS(t, "INFO {\"server_id\":\"x\"}\r\n")
	http := fakeNATS(t, "HTTP/1.1 400 Bad Request\r\n")

	tests := []struct {
		name    string
		url     string
		needJS  bool
		wantErr bool
	}{
		{"jetstream enabled", js, true, false},
		{"jetstream missing but required", noJS, true, true},
		{"jetstream missing, not required", noJS, false, false},
		{"not nats", http, false, true},
		{"nothing listening", "nats://127.0.0.1:1", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Ping(context.Background(), []string{tt.url}, tt.needJS)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPingSecondServer(t *testing.T) {
	js := fakeNATS(t, "INFO {\"jetstream\":true}\r\n")
	if err := Ping(context.Background(), []string{"nats://127.0.0.1:1", js}, true); err != nil {
		t.Fatal(err)
	}
}

package adapters

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/yourorg/soar/internal/domain"
)

func runKafka(t *testing.T, conn *domain.Connection, c *collect, want int) {
	t.Helper()
	src := domain.Source{Name: "kafka-src", Config: domain.JSON{"topics": []any{"alerts"}, "start": "earliest", "group": "g1"}}
	st, err := newKafka(src, StreamDeps{Conn: conn, Resolve: func(string) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- st.Run(ctx, c.emit) }()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && len(c.bodies()) < want {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let a wrongly re-read record show up and the commit land
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A consumer group reads every record, retries a refused batch without committing,
// commits after queueing, and a restarted consumer resumes after the committed offsets.
func TestKafkaConsumesAndCommitsAfterQueueing(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(2, "alerts"))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	var brokers []any
	for _, a := range cluster.ListenAddrs() {
		brokers = append(brokers, a)
	}
	prod, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	ctx := context.Background()
	produce := func(from, to int) {
		for i := from; i < to; i++ {
			if err := prod.ProduceSync(ctx, &kgo.Record{Topic: "alerts", Key: []byte(fmt.Sprint(i)), Value: []byte(fmt.Sprintf(`{"n":%d}`, i))}).FirstErr(); err != nil {
				t.Fatal(err)
			}
		}
	}
	produce(0, 10)
	conn := &domain.Connection{Name: "kafka-dev", ConnectorID: "kafka", Config: domain.JSON{"brokers": brokers}}

	first := &collect{refuse: 1}
	runKafka(t, conn, first, 10)
	got := first.bodies()
	sort.Strings(got)
	if len(got) != 10 {
		t.Fatalf("first run: %d records %v", len(got), got)
	}
	first.mu.Lock()
	ids := map[string]bool{}
	for _, m := range first.msgs {
		ids[m.ID] = true
		if m.Meta["transport"] != "kafka" || m.Meta["topic"] != "alerts" || m.Meta["key"] == "" {
			t.Errorf("meta %v", m.Meta)
		}
	}
	calls := first.calls
	first.mu.Unlock()
	if len(ids) != 10 || calls < 2 {
		t.Errorf("ids %d (want 10 distinct), emit calls %d (the refused batch must be retried)", len(ids), calls)
	}

	produce(10, 11)
	second := &collect{}
	runKafka(t, conn, second, 1)
	if b := second.bodies(); len(b) != 1 || b[0] != `{"n":10}` {
		t.Errorf("after a restart only the new record must arrive, got %v", b)
	}
}

func TestKafkaConfigValidation(t *testing.T) {
	for name, c := range map[string]struct {
		cfg  domain.JSON
		conn *domain.Connection
	}{
		"no topics":   {domain.JSON{}, nil},
		"bad start":   {domain.JSON{"topics": []any{"a"}, "start": "middle"}, nil},
		"no brokers":  {domain.JSON{"topics": []any{"a"}}, &domain.Connection{Name: "k", Config: domain.JSON{}}},
		"bad sasl":    {domain.JSON{"topics": []any{"a"}}, &domain.Connection{Name: "k", Config: domain.JSON{"brokers": "b:9092", "sasl": "kerberos"}, SecretRefs: domain.JSON{"username": "u", "password": "p"}}},
		"sasl no ref": {domain.JSON{"topics": []any{"a"}}, &domain.Connection{Name: "k", Config: domain.JSON{"brokers": "b:9092", "sasl": "plain"}}},
	} {
		deps := StreamDeps{Conn: c.conn, Resolve: func(string) (string, error) { return "x", nil }}
		if _, err := newKafka(domain.Source{Config: c.cfg}, deps); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// validation without a connection (the API) needs only the source settings
	if _, err := newKafka(domain.Source{Config: domain.JSON{"topics": "a,b"}}, StreamDeps{}); err != nil {
		t.Errorf("source-only validation: %v", err)
	}
}

package memory

import (
	"testing"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/storetest"
)

func TestMemoryRepoSatisfiesStateSuite(t *testing.T) {
	storetest.RunStateSuite(t, func(_ *testing.T, maxRetries int) store.Repo[domain.Case] {
		r := NewRepo[domain.Case]("cases")
		r.MaxRetries = maxRetries
		return r
	})
}

func TestMemoryRepoSatisfiesTimeSeriesSuite(t *testing.T) {
	storetest.RunTimeSeriesSuite(t, func(*testing.T) store.Repo[domain.Alert] { return NewRepo[domain.Alert]("alerts") })
}

func TestNewStoreHasEveryRepo(t *testing.T) {
	s := NewStore("soar")
	if s.Workflows == nil || s.Alerts == nil || s.Connections == nil || s.Names.Index("alerts") != "soar-alerts" {
		t.Fatal("incomplete store")
	}
}

package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAddListPersistAndCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.limit = 2
	for i := 0; i < 3; i++ {
		if err := s.Add(Transfer{
			ID:        string(rune('a' + i)),
			Direction: "out",
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	list := s.List(10)
	if len(list) != 2 {
		t.Fatalf("len %d", len(list))
	}
	if list[0].ID != "c" || list[1].ID != "b" {
		t.Fatalf("order %+v", list)
	}
	if err := s.Update("c", func(t *Transfer) {
		t.Status = "success"
		t.FinishedAt = time.Now().UTC()
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("c")
	if !ok || got.Status != "success" {
		t.Fatalf("get %+v %v", got, ok)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.List(10)) != 2 {
		t.Fatal("reload lost items")
	}
}

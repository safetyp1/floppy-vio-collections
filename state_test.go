package main

import (
	"path/filepath"
	"testing"
)

func TestStatePersistsAndExcludesSecondWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenState(path); err == nil {
		other.Close()
		t.Fatal("second worker acquired lock")
	}
	value := BindingState{CollectionID: "collection", Fingerprint: "fingerprint", Creating: true}
	if err := state.Put("binding:test", value); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	var restored BindingState
	found, err := state.Get("binding:test", &restored)
	if err != nil || !found || restored.CollectionID != value.CollectionID || !restored.Creating {
		t.Fatalf("%+v %v", restored, err)
	}
}

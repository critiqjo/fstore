package main

import (
	"github.com/critiqjo/cs733/assignment4/raft"
	"log"
	"os"
	"reflect"
	"testing"
)

func initPster(t *testing.T, dbpath string) *SimplePster {
	errlog := log.New(os.Stderr, "-- ", log.Lshortfile)
	pster, err := NewPster(dbpath, errlog)
	if err != nil {
		t.Fatal("Creating persister failed:", err)
	}
	return pster
}

func TestSimplePster(t *testing.T) {
	dbpath := "/tmp/testdb.gkv"
	pster := initPster(t, dbpath)

	entry := raft.RaftEntry{Term: 0, CEntry: nil}
	ok := pster.LogUpdate(0, []raft.RaftEntry{entry})
	if !ok {
		t.Fatal("Failed to persist log entry")
	}

	pster_dup := initPster(t, dbpath)
	idx, entry_dup := pster_dup.LastEntry()
	if idx != 0 || !reflect.DeepEqual(entry_dup, &entry) {
		t.Fatal("Changes were not synced with disk!")
	}
	pster_dup.Close()

	entry.Term = 1
	entries := []raft.RaftEntry{entry, entry, entry}
	entries[1].CEntry = &raft.ClientEntry{UID: 1234, Data: "Yo!"}
	ok = pster.LogUpdate(1, entries)
	if !ok {
		t.Fatal("Failed to persist log entry")
	}

	fields := raft.RaftFields{Term: 20, VotedFor: 9}
	ok = pster.SetFields(fields)
	if !ok {
		t.Fatal("Failed to persist fields")
	}

	pster_dup = initPster(t, dbpath)
	entries_dup, ok := pster_dup.LogSlice(1, 4)
	if !ok || !reflect.DeepEqual(entries_dup, entries) {
		t.Fatal("Changes were not synced with disk!")
	}

	fields_dup := pster_dup.GetFields()
	if !reflect.DeepEqual(fields_dup, &fields) {
		t.Fatal("Changes were not synced with disk!")
	}
	pster_dup.Close()
}

package main

import "testing"

func TestSimple(t *testing.T) { // {{{1
    _, err := NewMsger(1, map[int]Node {
        1: Node { Host: "127.0.0.1", RPort: 1234, CPort: 1235 },
        2: Node { Host: "127.0.0.1", RPort: 2345, CPort: 2346 },
        3: Node { Host: "127.0.0.1", RPort: 3456, CPort: 3457 },
    })
    if err != nil {
        t.Fatal("Creating messenger failed:", err)
    }
}
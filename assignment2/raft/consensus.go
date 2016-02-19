package raft

import (
    err "log" // avoid confusion
    "os"
    "time"
)

// Note: Raft state machine is a single-threaded event-loop
//       All events including timeouts are received on a single channel

type RaftNode struct { // FIXME organize differently?
    id int // node id - need not be in the range of 0..size
    size int // cluster size - too simplistic to support config. changes?
    // persistent fields
    log []RaftEntry
    term uint64
    votedFor int
    // volatile fields
    state RaftState
    commitIdx uint64
    lastAppld uint64
    // state-specific fields
    voteCount int // candidate
    nextIdx []uint64 // leader
    matchIdx []uint64 // leader
    // extras
    uidIdxMap map[uint64]uint64 // uid -> idx map for entries not yet applied
    timer *RaftTimer
    // links
    notifch chan Message
    msger Messenger
    pster Persister
    machn Machine
    // error logging
    err *err.Logger
}

func (self *RaftNode) isUpToDate(r *VoteRequest) bool {
    log := self.log
    lastEntry := log[len(log) - 1]
    return r.LastLogTerm > lastEntry.Term || (r.LastLogTerm == lastEntry.Term &&
                                              r.LastLogIdx >= lastEntry.Index)
}

func (self *RaftNode) logAppend(at int, entries []RaftEntry) {
    log := self.log
    // assert log[at - 1].Index + 1 == entries[0].Index
    log = append(log[:at], entries...)
    self.pster.LogUpdate(log[at:])
    self.log = log
}

func (self *RaftNode) setTermAndVote(term uint64, vote int) {
    self.term = term
    self.votedFor = vote
    self.pster.StatusSave(RaftFields { Term: term, VotedFor: vote })
}

func (self *RaftNode) setVote(vote int) {
    self.votedFor = vote
    self.pster.StatusSave(RaftFields { Term: self.term, VotedFor: vote })
}

func (self *RaftNode) timerReset() {
    self.timer.Reset(self.state)
}

func NewRaftNode(nodeId int, clusterSize int, msger Messenger, pster Persister, machn Machine) RaftNode {
    s := pster.StatusLoad()
    var term uint64
    var votedFor int
    if s == nil {
        term = 0
        votedFor = -1
    } else {
        term = s.Term
        votedFor = s.VotedFor
    }
    notifch := make(chan Message, 64)
    msger.Register(notifch)
    log := pster.LogRead()
    if log == nil {
        // simplification: to avoid a few checks for empty log
        log = []RaftEntry { RaftEntry { 0, 0, nil } }
    }
    return RaftNode {
        id: nodeId,
        size: clusterSize, // TODO read from pster
        log: log,
        term: term,
        votedFor: votedFor,
        state: Follower,
        commitIdx: 0,
        lastAppld: 0,
        voteCount: 0,
        nextIdx: nil,
        matchIdx: nil,
        uidIdxMap: make(map[uint64]uint64),
        timer: nil,
        notifch: notifch,
        msger: msger,
        pster: pster,
        machn: machn,
        err: err.New(os.Stderr, "err: ", err.Lshortfile),
    }
}

// Run the event loop, waits for messages and timeouts
func (self *RaftNode) Run(timeoutSampler func(RaftState) time.Duration) {
    self.timer = NewRaftTimer(func(v uint64) func() {
        return func() {
            self.notifch <- &timeout { v }
        }
    }, timeoutSampler)

    self.timerReset()

    loop:
    for {
        msg := <-self.notifch

        switch m := msg.(type) {
        case *timeout:
            if !self.timer.Match(m.version) { continue loop }
        case *exitLoop:
            break loop
        case *testEcho:
            self.msger.Send(self.id, m)
            continue loop
        }

        switch self.state {
        case Follower:
            self.followerHandler(msg)
        case Candidate:
            self.candidateHandler(msg)
        case Leader:
            self.leaderHandler(msg)
        }
    }
}

// Exit the event loop
func (self *RaftNode) Exit() {
    self.notifch <- &exitLoop { }
}

func (self *RaftNode) followerHandler(m Message) {
    switch msg := m.(type) {
    case *AppendEntries:
        if msg.Term < self.term {
            self.msger.Send(msg.LeaderId, &AppendReply { self.term, false, self.id })
        } else {
            if msg.Term > self.term {
                self.setTermAndVote(msg.Term, msg.LeaderId) // to track leaderId
            }

            log := self.log
            prevIdx := msg.PrevLogIdx
            firstIdx := log[0].Index
            prevOff := int(prevIdx - firstIdx)
            // assert prevOff >= 0
            if int(prevOff) < len(log) && log[prevOff].Term == msg.PrevLogTerm {
                if len(msg.Entries) > 0 {
                    self.logAppend(prevOff + 1, msg.Entries)
                }
                self.msger.Send(msg.LeaderId, &AppendReply { self.term, true, self.id })
                if self.commitIdx < msg.CommitIdx {
                    lastIdx := firstIdx + uint64(len(log) - 1)
                    pracCommitIdx := msg.CommitIdx
                    if pracCommitIdx > lastIdx {
                        pracCommitIdx = lastIdx
                    }
                    self.commitIdx = pracCommitIdx
                    if self.lastAppld < pracCommitIdx {
                        from, to := self.lastAppld + 1, pracCommitIdx + 1
                        clientEntries := make([]ClientEntry, to - from)
                        ci := 0
                        for idx := from; idx < to; idx += 1 {
                            cEntry := log[idx - firstIdx].Entry
                            if cEntry != nil {
                                clientEntries[ci] = *cEntry
                                ci += 1
                            }
                        }
                        if ci > 0 {
                            self.machn.ApplyLazy(clientEntries[:ci])
                        }
                        self.lastAppld = self.commitIdx
                    }
                } // else don't panic!
            } else {
                self.msger.Send(msg.LeaderId, &AppendReply { self.term, false, self.id })
            }
            self.timerReset()
        }

    case *VoteRequest:
        if msg.Term < self.term {
            self.msger.Send(msg.CandidId, &VoteReply { self.term, false })
        } else {
            if msg.Term > self.term {
                self.setTermAndVote(msg.Term, -1)
            }

            if self.votedFor >= 0 {
                self.msger.Send(msg.CandidId, &VoteReply { self.term, false })
            } else if !self.isUpToDate(msg) {
                self.msger.Send(msg.CandidId, &VoteReply { self.term, false })
            } else {
                self.setVote(msg.CandidId)
                self.msger.Send(msg.CandidId, &VoteReply { self.term, true })
                self.timerReset()
            }
        }

    case *AppendReply:
        break

    case *VoteReply:
        break

    case *ClientEntry:
        if self.votedFor > -1 {
            self.msger.Client301(msg.UID, self.votedFor)
        } else {
            self.msger.Client503(msg.UID)
        }

    case *timeout:
        self.state = Candidate
        self.candidateHandler(msg)

    default:
        self.err.Print("bad type: ", m)
    }
}

func (self *RaftNode) candidateHandler(m Message) {
    switch msg := m.(type) {
    case *AppendEntries:
        if msg.Term < self.term {
            self.msger.Send(msg.LeaderId, &AppendReply { self.term, false, self.id })
        } else {
            self.setVote(msg.LeaderId) // just needs to be non-zero
            self.state = Follower
            self.followerHandler(msg)
        }

    case *VoteRequest:
        if msg.Term <= self.term {
            self.msger.Send(msg.CandidId, &VoteReply { self.term, false })
        } else {
            self.state = Follower
            self.followerHandler(msg)
            //reset timer?
        }

    case *AppendReply:
        break

    case *VoteReply:
        if msg.Term == self.term && msg.Granted {
            self.voteCount += 1
            if self.voteCount > self.size / 2 {
                self.matchIdx = make([]uint64, self.size)
                lastIdx := self.log[len(self.log) - 1].Index
                self.nextIdx = make([]uint64, self.size)
                for i := range self.nextIdx {
                    self.nextIdx[i] = lastIdx
                }
                self.state = Leader
                self.timerReset()
            }
        } else if msg.Term > self.term {
            self.setTermAndVote(msg.Term, -1)
            self.state = Follower
        }

    case *ClientEntry:
        self.msger.Client503(msg.UID)

    case *timeout:
        self.setTermAndVote(self.term + 1, self.id)
        lastI := len(self.log) - 1
        self.msger.BroadcastVoteRequest(&VoteRequest {
            self.term,
            self.id,
            self.log[lastI].Index,
            self.log[lastI].Term,
        })
        self.timerReset()

    default:
        self.err.Print("bad type: ", m)
    }
}

func (self *RaftNode) leaderHandler(m Message) {
    switch m.(type) {
    case *AppendEntries:
        break
    case *VoteRequest:
        break
    case *AppendReply:
        break
    case *VoteReply:
        break
    case *ClientEntry:
        break
    case *timeout:
        break
    default:
        self.err.Print("bad type: ", m)
    }
}

// 3 internal Message-s
type timeout struct { version uint64 }
type exitLoop struct { }
type testEcho struct { }
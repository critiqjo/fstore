package raft

import (
    "errors"
    golog "log" // avoid confusion
    "math/rand"
    "sort"
    "time"
)

// Note: Raft state machine is a single-threaded event-loop
//       All events including timeouts are received on a single channel

type RaftNode struct { // FIXME organize differently?
    id uint32 // node id
    peerIds []uint32
    // persistent fields
    term uint64
    votedFor uint32
    // volatile fields
    state RaftState
    commitIdx uint64
    lastAppld uint64
    // state-specific fields
    voteSet map[uint32]bool // candidate: used as a set -- bool values are not used
    nextIdx map[uint32]uint64 // leader
    matchIdx map[uint32]uint64 // leader
    // extras
    idxOfUid map[uint64]uint64 // uid -> idx map for entries not yet applied
    timer *RaftTimer
    // links
    notifch chan Message
    msger Messenger
    pster Persister
    machn Machine
    // error logging
    err *golog.Logger
}

func NewNode( // {{{1
    selfId uint32, nodeIds []uint32, notifbuf int,
    msger Messenger, pster Persister, machn Machine,
    errlog *golog.Logger,
) (*RaftNode, error) {
    rf := pster.GetFields()
    var peerIds []uint32
    if len(nodeIds) < 3 {
        return nil, errors.New("Not enough nodes!")
    } else {
        var pSet = make(map[uint32]bool)
        var selfFound bool = false
        for _, peerId := range nodeIds {
            if peerId == NilNode {
                return nil, errors.New("NilNode = ^uint32(0) is a reserved nodeId")
            } else if peerId == selfId {
                selfFound = true
            } else {
                pSet[peerId] = true
            }
        }
        if !selfFound {
            return nil, errors.New("nodeIds should contain selfId")
        }
        for peerId := range pSet {
            peerIds = append(peerIds, peerId)
        }
        if len(peerIds) + 1 != len(nodeIds) {
            return nil, errors.New("nodeIds should not have duplicates")
        }
    }
    if rf == nil {
        rf = &RaftFields { 0, NilNode }
    }
    if idx, entry := pster.LastEntry(); idx == 0 && entry == nil {
        ok := pster.LogUpdate(0, []RaftEntry { RaftEntry { 0, nil } })
        if !ok { return nil, errors.New("Initial log update failed") }
    }
    notifch := make(chan Message, notifbuf)
    msger.Register(notifch)
    return &RaftNode {
        id: selfId,
        peerIds: peerIds,
        term: rf.Term,
        votedFor: rf.VotedFor,
        state: Follower,
        commitIdx: 0,
        lastAppld: 0,
        voteSet: nil,
        nextIdx: nil,
        matchIdx: nil,
        idxOfUid: nil,
        timer: nil,
        notifch: notifch,
        msger: msger,
        pster: pster,
        machn: machn,
        err: errlog,
    }, nil
}

// Run the event loop with default timeout logic
func (self *RaftNode) Run(timeoutBase time.Duration) { // {{{1
    followMinTO := 2 * timeoutBase
    candidMinTO := 3 * timeoutBase
    fuzz := int64(2 * timeoutBase)
    self.RunEx(func(state RaftState) time.Duration {
        switch state {
        case Follower:
            return followMinTO + time.Duration(rand.Int63n(fuzz))
        case Candidate:
            return candidMinTO + time.Duration(rand.Int63n(fuzz))
        case Leader:
            return timeoutBase
        }
        panic("Unreachable")
    })
}

// Run the event loop with custom timout sampling
func (self *RaftNode) RunEx(timeoutSampler func(RaftState) time.Duration) { // {{{1
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
func (self *RaftNode) Exit() { // {{{1
    self.notifch <- &exitLoop { }
}

// ---- private utility methods {{{1
func (self *RaftNode) log(idx uint64) *RaftEntry {
    return self.pster.Entry(idx)
}

func (self *RaftNode) logTail() (uint64, *RaftEntry) {
    return self.pster.LastEntry()
}

func (self *RaftNode) applyCommitted() {
    if self.lastAppld < self.commitIdx {
        var cEntries []ClientEntry
        for idx := self.lastAppld + 1; idx <= self.commitIdx; idx += 1 {
            cEntry := self.log(idx).CEntry
            if cEntry != nil {
                cEntries = append(cEntries, *cEntry)
                delete(self.idxOfUid, cEntry.UID)
            }
        }
        if len(cEntries) > 0 {
            self.machn.Execute(cEntries)
        }
        self.lastAppld = self.commitIdx
    }
}

func (self *RaftNode) isUpToDate(r *VoteRequest) bool {
    lastIdx, lastEntry := self.logTail()
    return r.LastLogTerm > lastEntry.Term || (r.LastLogTerm == lastEntry.Term && r.LastLogIdx >= lastIdx)
}

func (self *RaftNode) logUpdate(startIdx uint64, entries []RaftEntry) {
    if ok := self.pster.LogUpdate(startIdx, entries); !ok {
        self.err.Print("fatal: unable to update log; ignoring!!!")
    }
}

func (self *RaftNode) leaderLogAppend(entry RaftEntry) {
    lastIdx, _ := self.logTail()
    newIdx := lastIdx + 1
    self.logUpdate(newIdx, []RaftEntry { entry })
    if entry.CEntry != nil {
        self.idxOfUid[entry.CEntry.UID] = newIdx
    }
    for nodeId := range self.nextIdx {
        nextIdx := self.nextIdx[nodeId]
        if nextIdx == newIdx {
            self.sendAppendEntries(nodeId, 1)
        }
    }
}

func (self *RaftNode) sendAppendEntries(nodeId uint32, num_entries int) {
    nextIdx := self.nextIdx[nodeId]
    entries, ok := self.pster.LogSlice(nextIdx, nextIdx + uint64(num_entries))
    if !ok {
        self.err.Print("fatal: log index out of bounds; ignoring!!!")
        return
    }
    self.msger.Send(nodeId, &AppendEntries {
        Term: self.term,
        LeaderId: self.id,
        PrevLogIdx: nextIdx - 1,
        PrevLogTerm: self.log(nextIdx - 1).Term,
        Entries: entries,
        CommitIdx: self.commitIdx,
    })
    self.nextIdx[nodeId] += uint64(len(entries))
}

func (self *RaftNode) setTermAndVote(term uint64, vote uint32) {
    self.term = term
    self.votedFor = vote
    ok := self.pster.SetFields(RaftFields { Term: term, VotedFor: vote })
    if !ok {
        self.err.Print("fatal: could not persist fields; ignoring!!!")
    }
}

func (self *RaftNode) setVote(vote uint32) {
    self.setTermAndVote(self.term, vote)
}

func (self *RaftNode) timerReset() {
    self.timer.Reset(self.state)
}

type idxSlice []uint64
func (l idxSlice) Len() int           { return len(l) }
func (l idxSlice) Swap(i, j int)      { l[i], l[j] = l[j], l[i] }
func (l idxSlice) Less(i, j int) bool { return l[i] < l[j] }

func (self *RaftNode) updateCommitIdx() {
    var matchIdx []uint64
    for _, idx := range self.matchIdx {
        matchIdx = append(matchIdx, idx)
    }
    sort.Sort(idxSlice(matchIdx))
    offset := len(self.peerIds) / 2
    if self.log(matchIdx[offset]).Term == self.term {
        self.commitIdx = matchIdx[offset] // assert monotonicity?
    }
}

func (self *RaftNode) followerHandler(m Message) { // {{{1
    switch msg := m.(type) {
    case *AppendEntries:
        if msg.Term < self.term {
            self.msger.Send(msg.LeaderId, &AppendReply {
                Term: self.term, Success: false,
                NodeId: self.id, LastModIdx: 0,
            })
        } else {
            if msg.Term > self.term {
                self.setTermAndVote(msg.Term, msg.LeaderId) // to track leaderId
            }

            lastIdx, _ := self.logTail()
            prevIdx := msg.PrevLogIdx
            if prevIdx <= lastIdx && self.log(prevIdx).Term == msg.PrevLogTerm {
                var lastModIdx uint64 = 0 // should be non-zero only for non-heartbeat
                if len(msg.Entries) > 0 { // not heartbeat!
                    self.logUpdate(prevIdx + 1, msg.Entries)
                    lastModIdx, _ = self.logTail()
                }
                self.msger.Send(msg.LeaderId, &AppendReply {
                    Term: self.term, Success: true,
                    NodeId: self.id, LastModIdx: lastModIdx,
                })
                if self.commitIdx < msg.CommitIdx {
                    lastIdx, _ := self.logTail()
                    pracCommitIdx := msg.CommitIdx
                    if pracCommitIdx > lastIdx {
                        pracCommitIdx = lastIdx
                    }
                    self.commitIdx = pracCommitIdx
                    self.applyCommitted()
                } // else don't panic!
            } else {
                self.msger.Send(msg.LeaderId, &AppendReply {
                    Term: self.term, Success: false,
                    NodeId: self.id, LastModIdx: 0,
                })
            }
            self.timerReset()
        }

    case *VoteRequest:
        if msg.Term < self.term {
            self.msger.Send(msg.CandidId, &VoteReply { self.term, false, self.id })
        } else {
            if msg.Term > self.term {
                self.setTermAndVote(msg.Term, NilNode)
            }

            if !self.isUpToDate(msg) || self.votedFor != NilNode {
                self.msger.Send(msg.CandidId, &VoteReply { self.term, false, self.id })
            } else {
                self.setVote(msg.CandidId)
                self.msger.Send(msg.CandidId, &VoteReply { self.term, true, self.id })
                self.timerReset()
            }
        }

    case *AppendReply:

    case *VoteReply:

    case *ClientEntry:
        if self.votedFor != NilNode {
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

func (self *RaftNode) candidateHandler(m Message) { // {{{1
    switch msg := m.(type) {
    case *AppendEntries:
        if msg.Term < self.term {
            self.msger.Send(msg.LeaderId, &AppendReply {
                Term: self.term, Success: false,
                NodeId: self.id, LastModIdx: 0,
            })
        } else {
            self.setVote(msg.LeaderId) // just needs to be non-zero
            self.state = Follower
            self.followerHandler(msg)
        }

    case *VoteRequest:
        if msg.Term <= self.term {
            self.msger.Send(msg.CandidId, &VoteReply { self.term, false, self.id })
        } else {
            self.state = Follower
            self.followerHandler(msg)
            //reset timer?
        }

    case *AppendReply:

    case *VoteReply:
        if msg.Term == self.term && msg.Granted {
            self.voteSet[msg.NodeId] = true
            // voteSet contains self vote too, but peerIds doesn't contain self id
            if len(self.voteSet) > (len(self.peerIds) + 1) / 2 {
                lastIdx, _ := self.logTail()
                self.idxOfUid = make(map[uint64]uint64)
                for idx := self.lastAppld + 1; idx <= lastIdx; idx += 1 {
                    // fill idxOfUid with unapplied requests
                    // FIXME since commitIdx is volatile, the first leader
                    //       after a whole-cluster failure will have to read
                    //       the entire log to make this map
                    entry := self.log(idx)
                    if entry.CEntry != nil {
                        self.idxOfUid[entry.CEntry.UID] = idx
                    }
                }
                self.matchIdx = make(map[uint32]uint64)
                self.nextIdx = make(map[uint32]uint64)
                for _, nodeId := range self.peerIds {
                    self.matchIdx[nodeId] = 0
                    self.nextIdx[nodeId] = lastIdx + 1
                }
                self.state = Leader
                self.leaderHandler(&timeout { 0 })
                // optimize by replicating an empty log entry of current term?
            }
        } else if msg.Term > self.term {
            self.setTermAndVote(msg.Term, NilNode)
            self.state = Follower
        }

    case *ClientEntry:
        self.msger.Client503(msg.UID)

    case *timeout:
        self.voteSet = make(map[uint32]bool)
        self.voteSet[self.id] = true
        self.setTermAndVote(self.term + 1, self.id)
        lastIdx, lastEntry := self.logTail()
        self.msger.BroadcastVoteRequest(&VoteRequest {
            self.term,
            self.id,
            lastIdx,
            lastEntry.Term,
        })
        self.timerReset()

    default:
        self.err.Print("bad type: ", m)
    }
}

func (self *RaftNode) leaderHandler(m Message) { // {{{1
    // FIXME too many AppendEntries! coordinate heartbeats with non-heartbeats
    switch msg := m.(type) {
    case *AppendEntries:
        if self.term == msg.Term {
            self.err.Print("fatal: two leaders of same term; ignoring!!!")
        }
        self.candidateHandler(msg)

    case *VoteRequest:
        self.candidateHandler(msg)

    case *AppendReply:
        nodeId := msg.NodeId
        if msg.Success == true {
            lastIdx, _ := self.logTail()
            if msg.LastModIdx > 0 {
                // ignore duplicate/out-of-order messages
                if msg.LastModIdx > self.matchIdx[nodeId] {
                    self.matchIdx[nodeId] = msg.LastModIdx
                    self.updateCommitIdx()
                    self.applyCommitted()
                }
            }
            if self.nextIdx[nodeId] <= lastIdx {
                self.sendAppendEntries(nodeId, 8)
            }
        } else if msg.Term == self.term { // log mismatch
            if self.nextIdx[nodeId] > self.matchIdx[nodeId] + 1 {
                self.nextIdx[nodeId] -= 1
            }
            self.sendAppendEntries(nodeId, 0)
        } else if msg.Term > self.term {
            self.setTermAndVote(msg.Term, NilNode)
            self.state = Follower
            self.timerReset()
        } // else outdated message?

    case *VoteReply:

    case *ClientEntry:
        if self.machn.TryRespond(msg.UID) {
            break
        } else if logIdx, ok := self.idxOfUid[msg.UID]; ok {
            if self.log(logIdx).CEntry.UID != msg.UID {
                // this can only happen if a log entry was rewritten,
                // but idxOfUid is reset when a candidate becomes leader
                self.err.Print("fatal: idxOfUid mismatch; ignoring!!!")
            }
            break
        }
        self.leaderLogAppend(RaftEntry { self.term, msg })

    case *timeout:
        for _, nodeId := range self.peerIds {
            self.sendAppendEntries(nodeId, 0)
        }
        self.timerReset()

    default:
        self.err.Print("bad type: ", m)
    }
}

// ---- internal Message-s {{{1
type timeout struct { version uint64 }
type exitLoop struct { }
type testEcho struct { }

// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	"fmt"
	"github.com/Connor1996/badger/y"
	"github.com/pingcap-incubator/tinykv/log"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
	"math/rand"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term         uint64
	Vote         uint64
	VoteTotal    uint64
	VoteReceived uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout       int
	electionTimeoutRandom int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64

	// ---- added -----
	peers []uint64
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	// Your Code Here (2A).

	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	raft := &Raft{}
	raft.id = c.ID
	raft.peers = c.peers
	raft.electionTimeout = c.ElectionTick
	raft.electionTimeoutRandom = c.ElectionTick + rand.Int()%c.ElectionTick
	raft.heartbeatTimeout = c.HeartbeatTick
	raft.State = StateFollower
	raft.RaftLog = newLog(c.Storage)
	raft.votes = make(map[uint64]bool)
	raft.msgs = make([]pb.Message, 0)
	raft.Prs = make(map[uint64]*Progress)
	state, configSt, err := c.Storage.InitialState()

	if c.peers == nil {
		// c.peers will be nil in the case of restart
		//in this case, raft.peers should be set by configSt.Nodes
		raft.peers = configSt.Nodes
		c.peers = configSt.Nodes

		raft.RaftLog.applied = max(c.Applied, raft.RaftLog.applied)
	}

	if err != nil {
		panic("something wrong")
	}
	raft.Term = state.Term
	raft.Vote = state.Vote
	raft.RaftLog.committed = state.Commit


	lastIdx := raft.RaftLog.LastIndex()
	for _, p := range raft.peers {
		// init Progroess for each peer
		raft.Prs[p] = &Progress{raft.RaftLog.lastIdxOfSnapshot, lastIdx + 1}
	}
	//state, _, err := c.Storage.InitialState()
	//if err != nil {
	//	panic("todo")
	//}

	return raft
}

func (r *Raft) GetId() uint64 {
	return r.id
}

func (r *Raft) SoftState() *SoftState {
	return &SoftState{
		r.Lead,
		r.State,
	}
}

func (r *Raft) HardState() pb.HardState {
	return pb.HardState{
		Term:   r.Term,
		Vote:   r.Vote,
		Commit: r.RaftLog.committed,
	}
}

func (r *Raft) sendHeartbeats() {
	for i := 0; i < len(r.peers); i++ {
		if r.peers[i] == r.id {
			continue
		}
		r.sendHeartbeat(r.peers[i])
	}
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		//Index:   r.RaftLog.LastIndex(),
	}

	r.msgs = append(r.msgs, msg)
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	r.electionElapsed++
	r.heartbeatElapsed++

	// send heartbeat to followers
	if r.State == StateLeader {
		if r.heartbeatElapsed >= r.heartbeatTimeout {
			r.heartbeatElapsed = 0
			r.sendHeartbeats()
		}
	} else {
		if r.electionElapsed >= r.electionTimeoutRandom {
			r.becomeCandidate()
			r.requestVotes()
		}
	}
}

// votes related start ------------------------------------
func (r *Raft) handleVoteRequest(m pb.Message) {
	response := pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		To:      m.From,
		From:    r.id,
		Reject:  true,
	}
	defer func() {
		r.msgs = append(r.msgs, response)
	}()

	// Follower can vote for those having the same term with it or have been voted by the follower in this term.
	// Candidate and leader only vote for those having higher term
	if (r.Term == m.Term && r.State == StateFollower && (r.Vote == 0 || r.Vote == m.From)) ||
		m.Term > r.Term {
		lastIndex := r.RaftLog.LastIndex()
		lastTerm, _ := r.RaftLog.Term(lastIndex)

		r.becomeFollower(m.Term, 0)
		if m.LogTerm > lastTerm || (m.LogTerm == lastTerm && m.Index >= lastIndex) {
			r.Vote = m.From
			response.Reject = false
		}
		response.Term = m.Term
	}
	response.Term = r.Term
}

func (r *Raft) requestVotes() {
	for i := 0; i < len(r.peers); i++ {
		if r.peers[i] == r.id {
			continue
		}
		r.requestVote(r.peers[i])
	}
}

func (r *Raft) requestVote(to uint64) {
	m := pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		To:      to,
		From:    r.id,
		Term:    r.Term,
	}
	lastIdx := r.RaftLog.LastIndex()
	lastTerm, err := r.RaftLog.Term(lastIdx)
	if err != nil {
		panic("something wrong")
	}
	m.LogTerm = lastTerm
	m.Index = lastIdx
	r.msgs = append(r.msgs, m)
}

func (r *Raft) handleVoteResponse(m pb.Message) {
	if m.Term == r.Term {
		r.VoteTotal++
		if !m.Reject {
			r.VoteReceived++
			if r.VoteReceived > uint64(len(r.peers)/2) {
				r.becomeLeader()
				return
			}
		}
		if r.VoteTotal == uint64(len(r.peers)) {
			r.becomeFollower(r.Term, 0)
		}
	} else if m.Term > r.Term {
		r.Term = m.Term
		r.becomeFollower(r.Term, 0)
	}
}

// votes related end ------------------------------------
//                   ------------------------------------

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.Term = term
	r.Lead = lead
	r.State = StateFollower
	r.Vote = 0
	r.electionElapsed = 0
	r.electionTimeoutRandom = r.electionTimeout + rand.Int()%r.electionTimeout
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.Term += 1
	r.VoteReceived = 1
	r.VoteTotal = 1
	r.Vote = r.id
	r.votes[r.id] = true
	r.State = StateCandidate
	r.electionElapsed = 0
	r.electionTimeoutRandom = r.electionTimeout + rand.Int()%r.electionTimeout

	if len(r.peers) == 1 {
		r.becomeLeader()
	}
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term

	fmt.Printf("[%v] becomes leader\n", r.id)

	r.State = StateLeader
	r.leadTransferee = None
	r.heartbeatElapsed = 0
	r.Lead = r.id

	entry := make([]*pb.Entry, 1)
	entry[0] = &pb.Entry{
		Term:  r.Term,
		Index: r.RaftLog.LastIndex() + 1,
	}
	m := pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		To:      r.id,
		From:    r.id,
		Entries: entry,
		Term:    r.Term,
	}
	r.handleMsgPropose(m)
	r.broadCastAE()
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	switch r.State {
	case StateFollower:
		switch m.MsgType {
		case pb.MessageType_MsgSnapshot:
			r.handleSnapshot(m)
		case pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgHup:
			r.becomeCandidate()
			r.requestVotes()
		case pb.MessageType_MsgRequestVote:
			r.handleVoteRequest(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		case pb.MessageType_MsgTimeoutNow:
			r.handleMsgTimeOut()
		case pb.MessageType_MsgTransferLeader:
			r.handleLeaderTransfer(m)
		}
	case StateCandidate:
		switch m.MsgType {
		case pb.MessageType_MsgSnapshot:
			r.handleSnapshot(m)
		case pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgHup:
			r.becomeCandidate()
			r.requestVotes()
		case pb.MessageType_MsgRequestVoteResponse:
			r.handleVoteResponse(m)
		case pb.MessageType_MsgRequestVote:
			r.handleVoteRequest(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		case pb.MessageType_MsgTimeoutNow:
			r.handleMsgTimeOut()
		case pb.MessageType_MsgTransferLeader:
			r.handleLeaderTransfer(m)
		}
	case StateLeader:
		switch m.MsgType {
		case pb.MessageType_MsgSnapshot:
			r.handleSnapshot(m)
		case pb.MessageType_MsgBeat:
			r.heartbeatElapsed = 0
			r.sendHeartbeats()
		case pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgPropose:
			r.handleMsgPropose(m)
			r.broadCastAE()
		case pb.MessageType_MsgAppendResponse:
			r.handleMsgAppendResponse(m)
		case pb.MessageType_MsgRequestVote:
			r.handleVoteRequest(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		case pb.MessageType_MsgHeartbeatResponse:
			r.handleHeartbeatResponse(m)
		case pb.MessageType_MsgTransferLeader:
			r.handleLeaderTransfer(m)
		}
	}
	return nil
}

//handleMsgTimeOut is called when the leader status is transferred to r.id
// Start compaign immediately to become the new leader
func (r *Raft) handleMsgTimeOut() {
	if r.State == StateCandidate {
		return
	}
	r.Step(pb.Message{From: r.id, To: r.id, MsgType: pb.MessageType_MsgHup})
}

func (r *Raft) containsPeer(peer uint64) bool {
	for _, id := range r.peers {
		if id == peer {
			return true
		}
	}
	return false
}

func (r *Raft) handleLeaderTransfer(m pb.Message) {
	if r.State != StateLeader {
		if r.Lead != None {
			r.Step(pb.Message{From: m.From, To: r.Lead, MsgType: pb.MessageType_MsgHup})
		}
		return
	}
	transfereeId := m.From
	if !r.containsPeer(transfereeId) {
		log.Infof("[%d] does not contains [%d], so, stop leader transfer", r.id, transfereeId)
		return
	}
	if transfereeId == r.id {
		log.Infof("[%d] transferring leader to itself can be ignored", r.id)
		return
	}
	if r.leadTransferee != None {
		if r.leadTransferee == transfereeId {
			log.Infof("[%d] leader transfer to [%d] is in progress...", r.id, transfereeId)
			return
		}
	}
	r.leadTransferee = transfereeId
	if r.RaftLog.LastIndex() == r.Prs[transfereeId].Match {
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgTimeoutNow,
			To: transfereeId,
			From: r.id,
		})
	} else {
		r.sendAppend(transfereeId)
	}
}

// related to append entries ----------- start --------------------

func (r *Raft) handleMsgAppendResponse(m pb.Message) {
	if !m.Reject {
		if m.Index <= r.Prs[m.From].Match {
			return
		}
		r.Prs[m.From].Match = m.Index
		r.Prs[m.From].Next = m.Index + 1

		if m.From == r.leadTransferee && r.Prs[m.From].Match == r.RaftLog.LastIndex() {
			r.msgs = append(r.msgs, pb.Message{
				MsgType: pb.MessageType_MsgTimeoutNow,
				To: m.From,
				From: r.id,
			})
		}

		msg := pb.Message{
			MsgType:   pb.MessageType_MsgAppend,
			Term:      r.Term,
			From:      r.id,
			To:        m.From,
			CommitUse: true,
		}

		if m.Index <= r.RaftLog.committed {
			msg.Commit = m.Index
			r.msgs = append(r.msgs, msg)
			return
		}
		if r.RaftLog.entries[r.RaftLog.realIdx(m.Index)].Term < r.Term {
			return
		}

		// record peers that has higher or equal match index than m.index
		peers := make([]uint64, 0)
		peers = append(peers, m.From)
		counts := 2 // own and m.from
		for _, p := range r.peers {
			if p == m.From || p == r.id {
				continue
			}
			if r.Prs[p].Match >= m.Index {
				counts++
				peers = append(peers, p)
			}
		}
		if counts > (len(r.peers) / 2) {
			r.RaftLog.committed = m.Index
			for _, p := range peers {
				msg.To = p
				msg.Commit = m.Index
				r.msgs = append(r.msgs, msg)
			}
		}

	} else {
		if m.ConflictTerm != 0 {
			searched := false
			nextIdx := uint64(0)
			// find the last one whose term is the conflict Term
			for _, log := range r.RaftLog.entries {
				if log.Term == m.ConflictTerm {
					searched = true
					nextIdx = log.Index + 1
				}
				if log.Term > m.ConflictTerm {
					break
				}
			}
			if searched {
				r.Prs[m.From].Next = nextIdx
			} else {
				r.Prs[m.From].Next = m.Index + 1
			}
		} else {
			// m.Index may be 0, however, prs.next should not be decreased
			r.Prs[m.From].Next = max(m.Index+1, r.Prs[m.From].Match+1)
			//r.Prs[m.From].Next = m.Index + 1
		}
		r.sendAppend(m.From)
	}
}

func (r *Raft) broadCastAE() {
	for i := 0; i < len(r.peers); i++ {
		if r.peers[i] == r.id {
			continue
		}
		r.sendAppend(r.peers[i])
	}
}

func (r *Raft) needSnapshot(nextIdx uint64) bool {
	// if nextIdx is less or equal to lastIdxOfSnapshot
	//then, we have truncated the log entries that the follower needs
	return nextIdx <= r.RaftLog.lastIdxOfSnapshot
}


// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).

	lastIdx := r.RaftLog.LastIndex()
	nextIdx := r.Prs[to].Next
	entries := make([]*pb.Entry, 0)
	var m pb.Message
	if r.needSnapshot(nextIdx) {
		// handle sending snapshot to followers

		// storage.Snapshot() will generate snapshot from disk.
		//it will need some time, so, in order not to block the go routine, we return
		//immediately if the snapshot has not been generated completely.
		//In the next run and when the snapshot has been generated, we send it to the follower
		snapshot, err := r.RaftLog.storage.Snapshot()
		if err != nil {
			return false
		}
		m = pb.Message{
			MsgType: pb.MessageType_MsgSnapshot,
			To: to,
			From: r.id,
			Term: r.Term,
			Snapshot: &snapshot,
		}
		// without updating this, the follower may request snapshotting infinitely
		r.Prs[to].Next = snapshot.Metadata.Index + 1
	} else {
		// handle normal log replication
		if nextIdx > lastIdx {
			return false
		}
		for logicalIdx := nextIdx; logicalIdx <= lastIdx; logicalIdx++ {
			entries = append(entries, &r.RaftLog.entries[r.RaftLog.realIdx(logicalIdx)])
		}

		term, err := r.RaftLog.Term(nextIdx - 1)
		if err != nil {
			panic("something wrong")
		}
		m = pb.Message{
			MsgType: pb.MessageType_MsgAppend,
			To:      to,
			From:    r.id,
			Term:    r.Term,
			LogTerm: term,        // last matched term
			Index:   nextIdx - 1, // last matched index
			Commit:  r.RaftLog.committed,
			Entries: entries,
		}
	}
	r.msgs = append(r.msgs, m)
	return true
}

func (r *Raft) handleMsgPropose(m pb.Message) {
	for _, entry := range m.Entries {
		entry.Term = r.Term
		entry.Index = r.RaftLog.LastIndex() + 1
		r.RaftLog.AppendEntry(*entry)
	}
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.RaftLog.LastIndex() + 1
	if len(r.peers) == 1 {
		r.RaftLog.committed = r.RaftLog.LastIndex()
	}

}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	if m.CommitUse {
		r.RaftLog.committed = max(r.RaftLog.committed, m.Commit)
		return
	}
	// Your Code Here (2A).
	reply := pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		From:    r.id,
		To:      m.From,
		Reject:  true,
	}
	defer func() {
		r.msgs = append(r.msgs, reply)
	}()

	// 1. refuse any append request whose term is less than r.term
	if m.Term < r.Term {
		reply.Term = r.Term
		return
	}
	r.Term = m.Term
	r.Lead = m.From
	reply.Term = r.Term
	if r.State != StateFollower {
		r.becomeFollower(m.Term, m.From)
	}

	var droppedEntries []pb.Entry
	if m.Index == r.RaftLog.lastIdxOfSnapshot &&
		m.LogTerm == r.RaftLog.lastTermOfSnapshot {
		droppedEntries = r.RaftLog.entries
		r.RaftLog.entries = make([]pb.Entry, 0)
		reply.Reject = false
	} else {
		for i := 0; i < len(r.RaftLog.entries); i++ {
			// 2. if log contains an entry whose term and index equals to
			//the m.index and m.term (which is the last one added last time),
			//set reject be false
			if r.RaftLog.entries[i].Index == m.Index &&
				r.RaftLog.entries[i].Term == m.LogTerm {
				reply.Reject = false
				droppedEntries = r.RaftLog.entries[i+1:]
				r.RaftLog.entries = r.RaftLog.entries[:i+1]
				// todo : for raftlog stable
			}
		}
	}

	if reply.Reject {
		// case: append fail
		// reply with the conflict term and index so that leader can adjust
		// the entries that are to be appended

		// If m.Index exceeds the lastIndex, set the last entry as conflict entry
		if m.Index > r.RaftLog.LastIndex() {
			reply.Index = r.RaftLog.LastIndex()
			reply.ConflictTerm = 0
			return
		}
		if len(r.RaftLog.entries) == 0 {
			reply.ConflictTerm = r.RaftLog.lastTermOfSnapshot
			return
		}
		// set the term of the conflict entry
		index := Max(0, r.RaftLog.realIdx(m.Index))
		reply.ConflictTerm = r.RaftLog.entries[index].Term
		// find the first index of that term to speed up matching
		for i := 0; i < len(r.RaftLog.entries); i++ {
			if r.RaftLog.entries[i].Term == reply.ConflictTerm {
				reply.Index = r.RaftLog.entries[i].Index - 1
				break
			}
		}
		return
	}
	if len(m.Entries) > 0 {
		// 3. if an existing entry conflicts with a new one(same index with different term),
		// delete the existing entry and all that follow it
		// If this append message is delayed, then there's a possibility
		//that m.entries can be a subset of droppedEntries
		subset := true
		if len(droppedEntries) <= len(m.Entries) {
			subset = false
		}
		for i := 0; i < Min(len(droppedEntries), len(m.Entries)); i++ {
			if droppedEntries[i].Index != m.Entries[i].Index ||
				droppedEntries[i].Term != m.Entries[i].Term {
				subset = false
				r.RaftLog.stabled = min(r.RaftLog.stabled, droppedEntries[i].Index-1)
				break
			}
		}
		// 4. append new entries
		// if new entries are the subset of dropped entries, we should
		// append dropped entries again rather than new entries
		if subset {
			r.RaftLog.entries = append(r.RaftLog.entries, droppedEntries...)
		} else {
			for _, ent := range m.Entries {
				r.RaftLog.entries = append(r.RaftLog.entries, *ent)
			}
		}
	} else {
		r.RaftLog.entries = append(r.RaftLog.entries, droppedEntries...)
	}

	reply.Index = r.RaftLog.LastIndex()

	lastNewEntIndex := m.Index
	if len(m.Entries) > 0 {
		lastNewEntIndex = m.Entries[len(m.Entries)-1].Index
	}
	// 5. update commit index
	if m.Commit > r.RaftLog.committed {
		// update to the leader's commitIndex, but should not exceed the index of last new entry
		// also, it should be larger than the current commit index
		r.RaftLog.committed = max(min(m.Commit, lastNewEntIndex), r.RaftLog.committed)
	}
}

// related to append entries ----------- end --------------------
//                           ------------------------------------

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		Reject:  false,
		To:      m.From,
		From:    r.id,
	}
	if m.Term < r.Term {
		msg.Reject = true
		msg.Term = r.Term

	} else {
		r.becomeFollower(m.Term, m.From)
		// use msg.Index to be the lastindex to pull the gap in log from the leader
		msg.Index = r.RaftLog.LastIndex()
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) handleHeartbeatResponse(m pb.Message) {
	if m.Reject {
		r.becomeFollower(m.Term, 0)
	} else {
		if m.Index < r.RaftLog.LastIndex() {
			r.sendAppend(m.From)
		}
	}
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
	y.Assert(m.Snapshot != nil)
	metaData := m.Snapshot.Metadata
	if metaData.Index <= r.RaftLog.LastIndex() {
		return
	}

	log.Infof("[%d] receives snapshot msg", r.id)

	r.RaftLog.lastIdxOfSnapshot = metaData.Index
	r.RaftLog.lastTermOfSnapshot = metaData.Term
	r.RaftLog.committed = metaData.Index
	r.RaftLog.applied = metaData.Index
	r.RaftLog.stabled = metaData.Index
	r.RaftLog.pendingSnapshot = m.Snapshot
	r.RaftLog.entries = nil
	r.peers = metaData.ConfState.Nodes
	for _, id := range metaData.ConfState.Nodes {
		r.Prs[id] = &Progress{
			Match: r.RaftLog.lastIdxOfSnapshot,
			Next:  r.RaftLog.lastIdxOfSnapshot + 1,
		}
	}
	r.becomeFollower(m.Term, m.From)
	//// response
	//resp := pb.Message{
	//	MsgType: pb.MessageType_MsgAppendResponse,
	//	From:    r.id,
	//	To:      m.From,
	//	Reject:  false,
	//	Term:    r.Term,
	//	Index:   r.RaftLog.LastIndex(),
	//}
	//r.msgs = append(r.msgs, resp)
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}

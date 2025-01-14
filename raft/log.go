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
	"fmt"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// RaftLog manage the log entries, its struct look like:
//
//  snapshot/first.....applied....committed....stabled.....last
//  --------|------------------------------------------------|
//                            log entries
//
// for simplify the RaftLog implement should manage all log entries
// that not truncated
type RaftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	committed uint64

	// applied is the highest log position that the application has
	// been instructed to apply to its state machine.
	// Invariant: applied <= committed
	applied uint64

	// log entries with index <= stabled are persisted to storage.
	// It is used to record the logs that are not persisted by storage yet.
	// Everytime handling `Ready`, the unstabled logs will be included.
	stabled uint64

	// all entries that have not yet compact.
	entries []pb.Entry

	// the incoming unstable snapshot, if any.
	// (Used in 2C)
	pendingSnapshot *pb.Snapshot

	// Your Data Here (2A).
	lastIdxOfSnapshot uint64
	lastTermOfSnapshot uint64
}

// newLog returns log using the given storage. It recovers the log
// to the state that it just commits and applies the latest snapshot.
func newLog(storage Storage) *RaftLog {
	// Your Code Here (2A).
	rlog := RaftLog{}
	rlog.entries = make([]pb.Entry, 0)
	rlog.storage = storage

	firstIdx, err1 := storage.FirstIndex()
	lastIdx, err2 := storage.LastIndex()
	entries, err3 := storage.Entries(firstIdx, lastIdx + 1)
	if err1 != nil || err2 != nil || err3 != nil {
		panic("something wrong")
	}
	rlog.entries = append(rlog.entries, entries...)
	rlog.stabled = lastIdx
	rlog.lastIdxOfSnapshot = firstIdx - 1
	rlog.lastTermOfSnapshot, _ = storage.Term(rlog.lastIdxOfSnapshot)

	// todo: 此处的storage为 PeerStorage 但类型断言时却出错 fix it
	// ans: 那是因为PeerStorage 有 storage没有的属性, 只能从 storage -> PeerStorage
	// 不能从 PeerStorage -> storage
	// todo: 此处虽获得了 commit, 但没有获得apply fix it
	initState, _, err := storage.InitialState()
	if err != nil {
		panic(err)
	}
	rlog.committed = initState.Commit
	rlog.applied = firstIdx - 1

	return &rlog
}

func (l *RaftLog) realIdx(logicalIdx uint64) int {
	if logicalIdx <= l.lastIdxOfSnapshot {
		return -1
	}
	realIdx := logicalIdx - l.lastIdxOfSnapshot - 1
	if realIdx >= uint64(len(l.entries)) {
		panic(fmt.Sprintf("something wrong logicalIdx %d last index %d", logicalIdx, l.LastIndex()))
	}
	return int(realIdx)
}

// We need to compact the log entries in some point of time like
// storage compact stabled log entries prevent the log entries
// grow unlimitedly in memory
func (l *RaftLog) maybeCompact() {
	// Your Code Here (2C).
	lastSnapIdx := l.lastIdxOfSnapshot
	storageFirstIdx, err := l.storage.FirstIndex()
	errPanic(err)
	if lastSnapIdx < storageFirstIdx - 1 {
		l.lastTermOfSnapshot, _ = l.Term(storageFirstIdx - 1)
		l.entries = l.entries[l.realIdx(storageFirstIdx) : ]
		l.lastIdxOfSnapshot = storageFirstIdx - 1
	} else {
		return
	}
}

func (l *RaftLog) hasUnstableEnts() bool {
	stabledIdx := l.stabled
	lastIdx := l.LastIndex()
	return stabledIdx < lastIdx
}

// unstableEntries return all the unstable entries
func (l *RaftLog) unstableEntries() []pb.Entry {
	// Your Code Here (2A).
	idx := l.realIdx(l.stabled) + 1
	ents := make([]pb.Entry, 0)
	for ; idx <= l.realIdx(l.LastIndex()); idx++ {
		ents = append(ents, l.entries[idx])
	}
	return ents
}


// call len(l.nextEnts) > 0 is heavy
func (l *RaftLog) hasNextEnts() bool {
	appliedIdx := l.realIdx(l.applied)
	commitIdx := l.realIdx(l.committed)
	return appliedIdx < commitIdx
}

// nextEnts returns all the committed but not applied entries
func (l *RaftLog) nextEnts() (ents []pb.Entry) {
	// Your Code Here (2A).
	appliedIdx := l.realIdx(l.applied)
	commitIdx := l.realIdx(l.committed)
	ents = make([]pb.Entry, 0)
	for idx := appliedIdx + 1; idx <= commitIdx; idx++ {
		ents = append(ents, l.entries[idx])
	}
	return ents
}

// LastIndex return the last index of the log entries
func (l *RaftLog) LastIndex() uint64 {
	// Your Code Here (2A).
	if (len(l.entries) == 0) {
		return l.lastIdxOfSnapshot
	}
	return l.entries[len(l.entries) - 1].Index
}

// Term return the term of the entry in the given index
func (l *RaftLog) Term(i uint64) (uint64, error) {
	// Your Code Here (2A).
	realIdx := l.realIdx(i)
	if realIdx == -1 {
		return l.lastTermOfSnapshot, nil
	}
	return l.entries[realIdx].Term, nil
}

// AppendNewEntries append new entries
func (l *RaftLog) AppendEntry(entry pb.Entry) {
	l.entries = append(l.entries, entry)
}

func Max(x, y int) int {
	if x > y {
		return x
	}
	return y
}

func Min(x, y int) int {
	if x < y {
		return x
	}
	return y
}
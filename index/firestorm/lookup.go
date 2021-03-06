//  Copyright (c) 2015 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package firestorm

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const channelBufferSize = 1000

type lookupTask struct {
	docID  []byte
	docNum uint64
}

type Lookuper struct {
	f         *Firestorm
	workChan  chan *lookupTask
	quit      chan struct{}
	closeWait sync.WaitGroup

	tasksQueued uint64
	tasksDone   uint64
}

func NewLookuper(f *Firestorm) *Lookuper {
	rv := Lookuper{
		f:        f,
		workChan: make(chan *lookupTask, channelBufferSize),
		quit:     make(chan struct{}),
	}
	return &rv
}

func (l *Lookuper) Notify(docNum uint64, docID []byte) {
	atomic.AddUint64(&l.tasksQueued, 1)
	l.workChan <- &lookupTask{docID: docID, docNum: docNum}
}

func (l *Lookuper) Start() {
	l.closeWait.Add(1)
	go l.run()
}

func (l *Lookuper) Stop() {
	close(l.quit)
	l.closeWait.Wait()
}

func (l *Lookuper) run() {
	for {

		select {
		case <-l.quit:
			logger.Printf("lookuper asked to quit")
			l.closeWait.Done()
			return
		case task, ok := <-l.workChan:
			if !ok {
				logger.Printf("lookuper work channel closed unexpectedly, stopping")
				return
			}
			l.lookup(task)
		}
	}
}

func (l *Lookuper) lookup(task *lookupTask) {
	reader, err := l.f.store.Reader()
	if err != nil {
		logger.Printf("lookuper fatal: %v", err)
		return
	}
	defer func() {
		if cerr := reader.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	prefix := TermFreqPrefixFieldTermDocId(0, nil, task.docID)
	logger.Printf("lookuper prefix - % x", prefix)
	docNums := make(DocNumberList, 0)
	err = visitPrefix(reader, prefix, func(key, val []byte) (bool, error) {
		logger.Printf("lookuper sees key % x", key)
		tfk, err := NewTermFreqRowKV(key, val)
		if err != nil {
			return false, err
		}
		docNum := tfk.DocNum()
		docNums = append(docNums, docNum)
		return true, nil
	})
	if err != nil {
		logger.Printf("lookuper fatal: %v", err)
		return
	}
	oldDocNums := make(DocNumberList, 0, len(docNums))
	for _, docNum := range docNums {
		if task.docNum == 0 || docNum < task.docNum {
			oldDocNums = append(oldDocNums, docNum)
		}
	}
	logger.Printf("lookup migrating '%s' - %d - oldDocNums: %v", task.docID, task.docNum, oldDocNums)
	l.f.compensator.Migrate(task.docID, task.docNum, oldDocNums)
	if len(oldDocNums) == 0 && task.docNum != 0 {
		// this was an add, not an update
		atomic.AddUint64(l.f.docCount, 1)
	} else if len(oldDocNums) > 0 && task.docNum == 0 {
		// this was a delete (and it previously existed)
		atomic.AddUint64(l.f.docCount, ^uint64(0))
	}
	atomic.AddUint64(&l.tasksDone, 1)
}

// this is not intended to be used publicly, only for unit tests
// which depend on consistency we no longer provide
func (l *Lookuper) waitTasksDone(d time.Duration) error {
	timeout := time.After(d)
	tick := time.Tick(100 * time.Millisecond)
	for {
		select {
		// Got a timeout! fail with a timeout error
		case <-timeout:
			return fmt.Errorf("timeout")
		// Got a tick, we should check on doSomething()
		case <-tick:
			queued := atomic.LoadUint64(&l.tasksQueued)
			done := atomic.LoadUint64(&l.tasksDone)
			if queued == done {
				return nil
			}
		}
	}
}

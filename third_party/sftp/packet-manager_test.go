package sftp

import (
	"encoding"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type _testSender struct {
	sent chan encoding.BinaryMarshaler
}

func newTestSender() *_testSender {
	return &_testSender{make(chan encoding.BinaryMarshaler)}
}

func (s _testSender) sendPacket(p encoding.BinaryMarshaler) error {
	s.sent <- p
	return nil
}

type fakepacket struct {
	reqid uint32
	oid   uint32
}

func fake(rid, order uint32) fakepacket {
	return fakepacket{reqid: rid, oid: order}
}

func (fakepacket) MarshalBinary() ([]byte, error) {
	return make([]byte, 4), nil
}

func (fakepacket) UnmarshalBinary([]byte) error {
	return nil
}

func (f fakepacket) id() uint32 {
	return f.reqid
}

type pair struct {
	in, out fakepacket
}

type orderedPair struct {
	in  orderedRequest
	out orderedResponse
}

// basic test
var ttable1 = []pair{
	{fake(0, 0), fake(0, 0)},
	{fake(1, 1), fake(1, 1)},
	{fake(2, 2), fake(2, 2)},
	{fake(3, 3), fake(3, 3)},
}

// outgoing packets out of order
var ttable2 = []pair{
	{fake(10, 0), fake(12, 2)},
	{fake(11, 1), fake(11, 1)},
	{fake(12, 2), fake(13, 3)},
	{fake(13, 3), fake(10, 0)},
}

// request ids are not incremental
var ttable3 = []pair{
	{fake(7, 0), fake(7, 0)},
	{fake(1, 1), fake(1, 1)},
	{fake(9, 2), fake(3, 3)},
	{fake(3, 3), fake(9, 2)},
}

// request ids are all the same
var ttable4 = []pair{
	{fake(1, 0), fake(1, 0)},
	{fake(1, 1), fake(1, 1)},
	{fake(1, 2), fake(1, 3)},
	{fake(1, 3), fake(1, 2)},
}

var tables = [][]pair{ttable1, ttable2, ttable3, ttable4}

func TestPacketManager(t *testing.T) {
	sender := newTestSender()
	s := newPktMgr(sender)

	for i := range tables {
		table := tables[i]
		orderedPairs := make([]orderedPair, 0, len(table))
		for _, p := range table {
			orderedPairs = append(orderedPairs, orderedPair{
				in:  orderedRequest{p.in, p.in.oid},
				out: orderedResponse{p.out, p.out.oid},
			})
		}
		for _, p := range orderedPairs {
			s.incomingPacket(p.in)
		}
		for _, p := range orderedPairs {
			s.readyPacket(p.out)
		}
		for _, p := range table {
			pkt := <-sender.sent
			id := pkt.(orderedResponse).id()
			assert.Equal(t, id, p.in.id())
		}
	}
	s.close()
}

func (p sshFxpRemovePacket) String() string {
	return fmt.Sprintf("RmPkt:%d", p.ID)
}
func (p sshFxpOpenPacket) String() string {
	return fmt.Sprintf("OpPkt:%d", p.ID)
}
func (p sshFxpWritePacket) String() string {
	return fmt.Sprintf("WrPkt:%d", p.ID)
}
func (p sshFxpClosePacket) String() string {
	return fmt.Sprintf("ClPkt:%d", p.ID)
}

// A slow WRITE must still run before the WRITE and the FSETSTAT sent after it.
func TestPacketManagerSequential(t *testing.T) {
	sender := newTestSender()
	s := newPktMgr(sender)
	s.sequential = true
	var mu sync.Mutex
	var done []uint32
	runWorker := func(ch chan orderedRequest) {
		go func() {
			for pkt := range ch {
				if pkt.id() == 1 {
					time.Sleep(50 * time.Millisecond)
				}
				mu.Lock()
				done = append(done, pkt.id())
				mu.Unlock()
				s.readyPacket(s.newOrderedResponse(&sshFxpStatusPacket{ID: pkt.id()}, pkt.orderID()))
			}
		}()
	}
	pktChan := s.workerChan(runWorker)
	for _, p := range []requestPacket{&sshFxpWritePacket{ID: 1}, &sshFxpWritePacket{ID: 2}, &sshFxpFsetstatPacket{ID: 3}} {
		pktChan <- s.newOrderedRequest(p)
	}
	for range 3 {
		<-sender.sent
	}
	close(pktChan)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []uint32{1, 2, 3}, done)
}

func TestPacketManagerSequentialWrites(t *testing.T) {
	sender := &_testSender{sent: make(chan encoding.BinaryMarshaler, 32)}
	s := newPktMgr(sender)
	s.sequentialWrites = true

	var (
		mu             sync.Mutex
		inFlightReads  int
		maxConcurrent  int
		events         []string
		readGate       = make(chan struct{})
		readsEntered   = make(chan struct{}, 4)
	)

	runWorker := func(ch chan orderedRequest) {
		go func() {
			for pkt := range ch {
				switch pkt.requestPacket.(type) {
				case *sshFxpLstatPacket, *sshFxpStatPacket, *sshFxpReadPacket, *sshFxpFstatPacket:
					mu.Lock()
					inFlightReads++
					if inFlightReads > maxConcurrent {
						maxConcurrent = inFlightReads
					}
					events = append(events, fmt.Sprintf("read-start:%d", pkt.id()))
					mu.Unlock()
					if pkt.id() <= 4 {
						readsEntered <- struct{}{}
						<-readGate
					}
					mu.Lock()
					inFlightReads--
					events = append(events, fmt.Sprintf("read-end:%d", pkt.id()))
					mu.Unlock()
				default:
					mu.Lock()
					if inFlightReads != 0 {
						t.Errorf("mutation %d started while %d reads were in flight", pkt.id(), inFlightReads)
					}
					events = append(events, fmt.Sprintf("mut-start:%d", pkt.id()))
					mu.Unlock()
					if pkt.id() == 5 {
						time.Sleep(30 * time.Millisecond)
					}
					mu.Lock()
					events = append(events, fmt.Sprintf("mut-end:%d", pkt.id()))
					mu.Unlock()
				}
				s.readyPacket(s.newOrderedResponse(&sshFxpStatusPacket{ID: pkt.id()}, pkt.orderID()))
			}
		}()
	}

	pktChan := s.workerChan(runWorker)
	packets := []requestPacket{
		&sshFxpLstatPacket{ID: 1},
		&sshFxpStatPacket{ID: 2},
		&sshFxpReadPacket{ID: 3},
		&sshFxpLstatPacket{ID: 4},
		&sshFxpWritePacket{ID: 5},
		&sshFxpFsetstatPacket{ID: 6},
		&sshFxpFstatPacket{ID: 7},
		&sshFxpLstatPacket{ID: 8},
	}
	for _, p := range packets {
		pktChan <- s.newOrderedRequest(p)
	}

	// All 4 leading reads must run concurrently before we release the gate.
	for range 4 {
		select {
		case <-readsEntered:
		case <-time.After(2 * time.Second):
			t.Fatal("leading read packets did not run concurrently")
		}
	}
	close(readGate)

	var gotIDs []uint32
	for range len(packets) {
		pkt := <-sender.sent
		gotIDs = append(gotIDs, pkt.(orderedResponse).id())
	}
	close(pktChan)

	assert.Equal(t, []uint32{1, 2, 3, 4, 5, 6, 7, 8}, gotIDs)
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, maxConcurrent, 4)

	idx := func(ev string) int {
		for i, got := range events {
			if got == ev {
				return i
			}
		}
		return -1
	}
	// Mutation 5 must start after all 4 reads finish, and finish before mutation 6 starts.
	for id := 1; id <= 4; id++ {
		assert.Less(t, idx(fmt.Sprintf("read-end:%d", id)), idx("mut-start:5"))
	}
	assert.Less(t, idx("mut-end:5"), idx("mut-start:6"))
	// Post-mutation reads 7 & 8 must start only after mutation 6 finishes.
	assert.Less(t, idx("mut-end:6"), idx("read-start:7"))
	assert.Less(t, idx("mut-end:6"), idx("read-start:8"))
}

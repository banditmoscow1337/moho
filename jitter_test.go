package audio

import (
	"encoding/binary"
	"testing"

	"github.com/banditmoscow1337/moho/voicebuf"
)

func pushTestPacket(jb *JitterBuffer, seq uint16) {
	p := voicebuf.Get()
	*p = (*p)[:3]
	binary.BigEndian.PutUint16((*p)[:2], seq)
	(*p)[2] = 0x42
	jb.Push(seq, p)
}

func popTestPacket(t *testing.T, jb *JitterBuffer, want uint16) {
	t.Helper()
	p := jb.Pop()
	if p == nil {
		t.Fatalf("no packet for sequence %d", want)
	}
	if got := binary.BigEndian.Uint16((*p)[:2]); got != want {
		t.Fatalf("popped sequence %d, want %d", got, want)
	}
	releasePacket(p)
}

func TestJitterReorderWrapAndLate(t *testing.T) {
	jb := NewJitterBuffer(3)
	defer jb.Cleanup()
	// The first received packet need not be the first packet sent.
	pushTestPacket(jb, 65535)
	pushTestPacket(jb, 65534)
	pushTestPacket(jb, 0)
	if got := jb.Level(); got != 3 {
		t.Fatalf("reordered level %d", got)
	}
	popTestPacket(t, jb, 65534)
	pushTestPacket(jb, 65534) // late
	pushTestPacket(jb, 0)     // duplicate
	popTestPacket(t, jb, 65535)
	popTestPacket(t, jb, 0)
	if stats := jb.Stats(); stats.LatePackets != 1 || stats.DuplicatePackets != 1 || stats.OverflowPackets != 0 {
		t.Fatalf("unexpected jitter counters: %+v", stats)
	}
	if p := jb.Pop(); p != nil {
		t.Fatal("empty buffer produced a packet")
	}
	// An idle receiver re-anchors to the next packet rather than waiting
	// for the sender to catch up with a stale playhead.
	for _, seq := range []uint16{200, 201, 202} {
		pushTestPacket(jb, seq)
	}
	popTestPacket(t, jb, 200)
}

func TestJitterWindowBoundedAndFastForward(t *testing.T) {
	jb := NewJitterBuffer(3)
	defer jb.Cleanup()
	for seq := 0; seq < 10000; seq++ {
		pushTestPacket(jb, uint16(seq))
		if n := jb.Level(); n > JitterBufferWindow || n < 0 {
			t.Fatalf("level %d after sequence %d", n, seq)
		}
	}
	if jb.Stats().OverflowPackets == 0 {
		t.Fatal("window overflow did not account for evicted frames")
	}
	// Playback picks the freshest frames after a long producer stall.
	popTestPacket(t, jb, 9997)
	popTestPacket(t, jb, 9998)
	popTestPacket(t, jb, 9999)
	if got := jb.Level(); got != 0 {
		t.Fatalf("unreleased frames after fast forward: %d", got)
	}
}

func TestIncomingKeepsWireBaseForSharedPool(t *testing.T) {
	e := NewEngineWithComponents(&testBackend{}, &testCodecs{})
	defer e.Cleanup()
	if err := e.AddPeer("peer"); err != nil {
		t.Fatal(err)
	}
	p := voicebuf.Get()
	*p = (*p)[:3]
	(*p)[0], (*p)[1], (*p)[2] = 0x12, 0x34, 0x56
	e.ProcessIncoming("peer", p)
	jb := e.inputs.Load().(map[string]*JitterBuffer)["peer"]
	jb.mu.Lock()
	slot := jb.slots[0x1234%JitterBufferWindow]
	jb.mu.Unlock()
	if slot.data != p || cap(*p) != voicebuf.PacketSize || len(*p) != 3 || (*p)[2] != 0x56 {
		t.Fatalf("wire base was truncated: len=%d cap=%d", len(*p), cap(*p))
	}
	e.RemovePeer("peer")
	if jb.Level() != 0 || !jb.closed {
		t.Fatal("peer removal retained buffered packets")
	}
}

func TestJitterDoesNotRetainOversizedBacking(t *testing.T) {
	jb := NewJitterBuffer(3)
	defer jb.Cleanup()
	large := make([]byte, 65535)
	packet := large[:3]
	jb.Push(7, &packet)
	jb.mu.Lock()
	stored := jb.slots[7%JitterBufferWindow].data
	jb.mu.Unlock()
	if stored == nil {
		t.Fatal("jitter lost packet")
	}
	if cap(*stored) != voicebuf.PacketSize {
		t.Fatalf("jitter retained large backing: cap=%d", cap(*stored))
	}
}

func TestJitterBufferNoPrematureBufferingOnDrainedQueue(t *testing.T) {
	jb := NewJitterBuffer(3)
	defer jb.Cleanup()

	// Pre-fill to target depth to exit initial buffering
	pushTestPacket(jb, 0)
	pushTestPacket(jb, 1)
	pushTestPacket(jb, 2)

	// Pop all 3 packets
	popTestPacket(t, jb, 0)
	popTestPacket(t, jb, 1)
	popTestPacket(t, jb, 2) // Queue count is now 0, but packet 2 is playing for 20ms

	// Packet 3 arrives on time before the next 20ms Pop callback
	pushTestPacket(jb, 3)

	// Pop should succeed immediately without locking into buffering mode
	p := jb.Pop()
	if p == nil {
		t.Fatal("Pop returned nil: buffer prematurely re-entered buffering on last packet")
	}
	if got := binary.BigEndian.Uint16((*p)[:2]); got != 3 {
		t.Fatalf("popped seq %d, want 3", got)
	}
	releasePacket(p)
}

func TestJitterBufferLatePacketAfterQueueDrainedDoesNotReanchor(t *testing.T) {
	jb := NewJitterBuffer(3)
	defer jb.Cleanup()

	// Fill and drain to count == 0
	pushTestPacket(jb, 10)
	pushTestPacket(jb, 11)
	pushTestPacket(jb, 12)

	popTestPacket(t, jb, 10)
	popTestPacket(t, jb, 11)
	popTestPacket(t, jb, 12)

	// Delayed packet 11 arrives after packet 12 was already popped
	pushTestPacket(jb, 11)

	if stats := jb.Stats(); stats.LatePackets != 1 {
		t.Fatalf("expected late packet counter = 1, got %d", stats.LatePackets)
	}

	// Next sequential packet 13 arrives
	pushTestPacket(jb, 13)

	// Playhead must remain at 12; packet 13 should be ready next
	popTestPacket(t, jb, 13)
}

func TestJitterBufferActualUnderrunRebuffers(t *testing.T) {
	jb := NewJitterBuffer(3)
	defer jb.Cleanup()

	pushTestPacket(jb, 0)
	pushTestPacket(jb, 1)
	pushTestPacket(jb, 2)

	popTestPacket(t, jb, 0)
	popTestPacket(t, jb, 1)
	popTestPacket(t, jb, 2)

	// True underrun: consumer asks for audio when count == 0
	if p := jb.Pop(); p != nil {
		t.Fatalf("expected nil on underrun, got packet")
	}

	// Buffer must now be in buffering state
	pushTestPacket(jb, 3)
	if p := jb.Pop(); p != nil {
		t.Fatalf("expected nil while rebuffering below targetDepth, got packet")
	}

	pushTestPacket(jb, 4)
	pushTestPacket(jb, 5) // Reached target depth (count == 3)

	popTestPacket(t, jb, 3)
}

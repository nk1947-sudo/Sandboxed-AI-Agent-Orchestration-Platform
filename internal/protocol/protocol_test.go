package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := ExecutionRequest{
		ID:         "req-1",
		Kind:       KindExec,
		Script:     "echo hello && echo world 1>&2",
		TimeoutSec: 5,
		Workdir:    "/workspace",
		Env:        map[string]string{"FOO": "bar"},
	}
	if err := WriteMessage(&buf, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got ExecutionRequest
	if err := ReadMessage(&buf, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, req) {
		t.Fatalf("round trip mismatch:\n got=%+v\nwant=%+v", got, req)
	}
}

func TestResponseStream(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	for _, f := range []ResponseFrame{
		{ID: "1", Kind: FrameOutput, Data: "hello "},
		{ID: "1", Kind: FrameOutput, Data: "world\n"},
		{ID: "1", Kind: FrameExit, ExitCode: 0},
	} {
		if err := fw.Write(f); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}

	var frames []ResponseFrame
	for {
		var f ResponseFrame
		err := ReadMessage(&buf, &f)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		frames = append(frames, f)
		if f.Terminal() {
			break
		}
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %+v", len(frames), frames)
	}
	if !frames[2].Terminal() || frames[2].Kind != FrameExit {
		t.Fatalf("last frame should be terminal exit, got %+v", frames[2])
	}
}

func TestReadMessageRejectsOversizeHeader(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(MaxFrameSize+1))
	buf.Write(hdr[:])

	var f ResponseFrame
	if err := ReadMessage(&buf, &f); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestWriteMessageRejectsOversizePayload(t *testing.T) {
	var buf bytes.Buffer
	big := ExecutionRequest{Script: string(make([]byte, MaxFrameSize+1))}
	if err := WriteMessage(&buf, big); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("nothing should be written for an oversize payload, wrote %d bytes", buf.Len())
	}
}

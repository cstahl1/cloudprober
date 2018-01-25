// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package external

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/google/cloudprober/logger"
	"github.com/google/cloudprober/probes/external/serverutils"
	"github.com/google/cloudprober/probes/options"
	"github.com/google/cloudprober/targets"
)

// stratProbeServer starts a test probe server to work with the TestProbeServer
// test below.
func startProbeServer(t *testing.T, testPayload string, r io.Reader, w io.WriteCloser) {
	for {
		req, err := serverutils.ReadProbeRequest(bufio.NewReader(r))
		if err != nil {
			t.Errorf("Error reading probe request. Err: %v", err)
			return
		}
		var action string
		opts := req.GetOptions()
		for _, opt := range opts {
			if opt.GetName() == "action" {
				action = opt.GetValue()
				break
			}
		}
		id := req.GetRequestId()

		actionToResponse := map[string]*serverutils.ProbeReply{
			"nopayload": &serverutils.ProbeReply{RequestId: proto.Int32(id)},
			"payload": &serverutils.ProbeReply{
				RequestId: proto.Int32(id),
				Payload:   proto.String(testPayload),
			},
			"payload_with_error": &serverutils.ProbeReply{
				RequestId:    proto.Int32(id),
				Payload:      proto.String(testPayload),
				ErrorMessage: proto.String("error"),
			},
		}
		t.Logf("Request id: %d, action: %s", id, action)
		if action == "pipe_server_close" {
			w.Close()
			return
		}
		if res, ok := actionToResponse[action]; ok {
			serverutils.WriteMessage(res, w)
		}
	}
}

func setProbeOptions(p *Probe, name, value string) {
	if p.c == nil {
		p.c = &ProbeConf{}
	}
	p.c.Options = []*ProbeConf_Option{
		{
			Name:  proto.String(name),
			Value: proto.String(value),
		},
	}
}

// runAndVerifyServerProbe executes a server probe and verifies the replies
// received.
func runAndVerifyProbe(t *testing.T, p *Probe, action string, wantError bool, payload string, total, success int64) error {
	setProbeOptions(p, "action", action)
	rep, err := p.runServerProbeForTarget(context.Background(), "dummy")
	if !wantError && err != nil {
		t.Errorf(err.Error())
	}
	if wantError && err == nil {
		t.Error("Expected error, but didn't get one")
	}
	if rep.GetPayload() != payload {
		t.Errorf("Got payload=%s, Want: %s", rep.GetPayload(), payload)
	}
	if p.total != total {
		t.Errorf("p.total=%d, Want: %d", p.total, total)
	}
	if p.success != success {
		t.Errorf("p.success=%d, Want: %d", p.success, success)
	}
	return err
}

func testProbeServerSetup(t *testing.T, readErrorCh chan error) (*Probe, string) {
	// We create two pairs of pipes to establish communication between this prober
	// and the test probe server (defined above).
	// Test probe server input pipe. We writes on w1 and external command reads
	// from r1.
	r1, w1, err := os.Pipe()
	if err != nil {
		t.Errorf("Error creating OS pipe. Err: %v", err)
	}
	// Test probe server output pipe. External command writes on w2 and we read
	// from r2.
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Errorf("Error creating OS pipe. Err: %v", err)
	}

	testPayload := "p90 45\n"
	// Start probe server in a goroutine
	go startProbeServer(t, testPayload, r1, w2)

	p := &Probe{
		opts: &options.Options{
			Targets: targets.StaticTargets("localhost"),
			Timeout: 5 * time.Second,
		},
		l:          &logger.Logger{},
		replyChan:  make(chan *serverutils.ProbeReply),
		cmdRunning: true, // don't try to start the probe server
		cmdStdin:   w1,
		cmdStdout:  r2,
	}
	// Start the goroutine that reads probe replies. We don't use the done
	// channel here. It's only to satisfy the readProbeReplies interface.
	done := make(chan struct{})
	go func() {
		err := p.readProbeReplies(done)
		if readErrorCh != nil {
			readErrorCh <- err
			close(readErrorCh)
		}
	}()

	return p, testPayload
}

func TestProbeServer(t *testing.T) {
	p, testPayload := testProbeServerSetup(t, nil)

	var total, success int64

	// No payload
	total++
	success++
	runAndVerifyProbe(t, p, "nopayload", false, "", total, success)

	// Payload
	total++
	success++
	runAndVerifyProbe(t, p, "payload", false, testPayload, total, success)

	// Payload with error
	total++
	runAndVerifyProbe(t, p, "payload_with_error", true, testPayload, total, success)

	// Timeout
	total++
	// Reduce probe timeout to make this test pass quicker.
	p.opts.Timeout = time.Second
	runAndVerifyProbe(t, p, "timeout", true, "", total, success)
}

func TestProbeServerRemotePipeClose(t *testing.T) {
	readErrorCh := make(chan error)
	p, _ := testProbeServerSetup(t, readErrorCh)

	var total, success int64
	// Remote pipe close
	total++
	// Reduce probe timeout to make this test pass quicker.
	p.opts.Timeout = time.Second
	runAndVerifyProbe(t, p, "pipe_server_close", true, "", total, success)
	readError := <-readErrorCh
	if readError == nil {
		t.Error("Didn't get error in reading pipe")
	}
	if readError != io.EOF {
		t.Errorf("Didn't get correct error in reading pipe. Got: %v, wanted: %v", readError, io.EOF)
	}
}

func TestProbeServerLocalPipeClose(t *testing.T) {
	readErrorCh := make(chan error)
	p, _ := testProbeServerSetup(t, readErrorCh)

	var total, success int64
	// Local pipe close
	total++
	// Reduce probe timeout to make this test pass quicker.
	p.opts.Timeout = time.Second
	p.cmdStdout.(*os.File).Close()
	runAndVerifyProbe(t, p, "pipe_local_close", true, "", total, success)
	readError := <-readErrorCh
	if readError == nil {
		t.Error("Didn't get error in reading pipe")
	}
	if _, ok := readError.(*os.PathError); !ok {
		t.Errorf("Didn't get correct error in reading pipe. Got: %T, wanted: *os.PathError", readError)
	}
}

func TestSubstituteLabels(t *testing.T) {
	tests := []struct {
		desc   string
		in     string
		labels map[string]string
		want   string
		found  bool
	}{
		{
			desc:  "No replacement",
			in:    "foo bar baz",
			want:  "foo bar baz",
			found: true,
		},
		{
			desc: "Replacement beginning",
			in:   "@foo@ bar baz",
			labels: map[string]string{
				"foo": "h e llo",
			},
			want:  "h e llo bar baz",
			found: true,
		},
		{
			desc: "Replacement middle",
			in:   "beginning @😿@ end",
			labels: map[string]string{
				"😿": "😺",
			},
			want:  "beginning 😺 end",
			found: true,
		},
		{
			desc: "Replacement end",
			in:   "bar baz @foo@",
			labels: map[string]string{
				"foo": "XöX",
				"bar": "nope",
			},
			want:  "bar baz XöX",
			found: true,
		},
		{
			desc: "Replacements",
			in:   "abc@foo@def@foo@ jk",
			labels: map[string]string{
				"def": "nope",
				"foo": "XöX",
			},
			want:  "abcXöXdefXöX jk",
			found: true,
		},
		{
			desc: "Multiple labels",
			in:   "xx @foo@@bar@ yy",
			labels: map[string]string{
				"bar": "_",
				"def": "nope",
				"foo": "XöX",
			},
			want:  "xx XöX_ yy",
			found: true,
		},
		{
			desc: "Not found",
			in:   "A b C @d@ e",
			labels: map[string]string{
				"bar": "_",
				"def": "nope",
				"foo": "XöX",
			},
			want: "A b C @d@ e",
		},
		{
			desc: "@@",
			in:   "hello@@foo",
			labels: map[string]string{
				"bar": "_",
				"def": "nope",
				"foo": "XöX",
			},
			want:  "hello@foo",
			found: true,
		},
		{
			desc: "odd number",
			in:   "hello@foo@bar@xx",
			labels: map[string]string{
				"foo": "yy",
			},
			want:  "helloyybar@xx",
			found: true,
		},
	}

	for _, tc := range tests {
		got, found := substituteLabels(tc.in, tc.labels)
		if tc.found != found {
			t.Errorf("%v: substituteLabels(%q, %q) = _, %v, want %v", tc.desc, tc.in, tc.labels, found, tc.found)
		}
		if tc.want != got {
			t.Errorf("%v: substituteLabels(%q, %q) = %q, _, want %q", tc.desc, tc.in, tc.labels, got, tc.want)
		}
	}
}

// TestSendRequest verifies that sendRequest sends appropriatly populated
// ProbeRequest.
func TestSendRequest(t *testing.T) {
	var buf bytes.Buffer
	p := &Probe{
		name: "testprobe",
		opts: &options.Options{
			Targets: targets.StaticTargets("localhost"),
		},
		l:        &logger.Logger{},
		cmdStdin: &buf,
	}
	setProbeOptions(p, "target", "@target@")
	requestID := int32(1234)
	target := "localhost"

	err := p.sendRequest(requestID, target)
	if err != nil {
		t.Errorf("Failed to sendRequest: %v", err)
	}
	req := new(serverutils.ProbeRequest)
	var length int
	_, err = fmt.Fscanf(&buf, "\nContent-Length: %d\n\n", &length)
	if err != nil {
		t.Errorf("Failed to read header: %v", err)
	}
	err = proto.Unmarshal(buf.Bytes(), req)
	if err != nil {
		t.Fatalf("Failed to Unmarshal probe Request: %v", err)
	}
	if got, want := req.GetRequestId(), requestID; got != requestID {
		t.Errorf("req.GetRequestId() = %q, want %v", got, want)
	}
	opts := req.GetOptions()
	if len(opts) != 1 {
		t.Errorf("req.GetOptions() = %q (%v), want only one item", opts, len(opts))
	}
	if got, want := opts[0].GetName(), "target"; got != want {
		t.Errorf("opts[0].GetName() = %q, want %q", got, want)
	}
	if got, want := opts[0].GetValue(), target; got != target {
		t.Errorf("opts[0].GetValue() = %q, want %q", got, want)
	}
}

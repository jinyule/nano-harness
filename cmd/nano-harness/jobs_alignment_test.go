package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestComposition_JobOutputKeepsDecodedLossAndStatus(t *testing.T) {
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "job_output", arguments: `{"job_id":"bash-1"}`},
		{text: "collected"},
	})
	var jobs *appJob.Service
	for _, candidate := range assembled.app.plugins {
		if service, ok := candidate.(*appJob.Service); ok {
			jobs = service
		}
	}
	if jobs == nil {
		t.Fatal("composition did not mount jobs")
	}
	wrote := make(chan struct{})
	_, err := jobs.Launch(appJob.Spec{Kind: "bash", Label: "invalid bytes", Owner: "session-plan", Foreground: true, Run: func(ctx context.Context, output *appJob.Output) appJob.Outcome {
		_, _ = output.Writer(appJob.Stdout).Write(bytes.Repeat([]byte{0xff, 'a'}, 63<<10))
		close(wrote)
		<-ctx.Done()
		return appJob.Outcome{Status: appJob.StatusKilled}
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-wrote
	if result := assembled.turn(t, "collect output"); result.Err != nil || result.Text != "collected" {
		t.Fatalf("turn = %+v", result)
	}
	requests := seen()
	if len(requests) != 2 {
		t.Fatalf("provider received %d requests", len(requests))
	}
	encoded, err := json.Marshal(requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(encoded)
	if !strings.Contains(wire, "some output was dropped from memory") || !strings.Contains(wire, "[status: running]") {
		t.Fatal("next model request omitted loss or status")
	}
	results := orderedToolResults(assembled.records(t))
	if len(results) != 1 || results[0].IsError || len(results[0].Output) > session.MaxTextBytes || !utf8.ValidString(results[0].Output) || !strings.Contains(results[0].Output, "[status: running]") || !strings.Contains(results[0].Output, "full output: (unavailable)") {
		t.Fatal("durable job output omitted the decoded loss envelope")
	}
}

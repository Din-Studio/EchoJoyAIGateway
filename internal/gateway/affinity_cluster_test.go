package gateway

import (
	"testing"

	"gpt-load/internal/cluster"
)

func TestSoftAffinityLearnedOnOneInstanceIsHitOnAnother(t *testing.T) {
	server, client := newGatewayClusterClient(t)
	forwarderA := &scriptedForwarder{results: successfulAffinityResults(2)}
	forwarderB := &scriptedForwarder{results: successfulAffinityResults(2)}
	handlerA, _, _ := newHandlerForTest(t, forwarderA, "sk-one", "sk-two")
	handlerB, _, _ := newHandlerForTest(t, forwarderB, "sk-one", "sk-two")
	handlerA.affinity = cluster.NewAffinity(client)
	handlerB.affinity = cluster.NewAffinity(client)
	sinkB := &recordingRequestLogSink{}
	handlerB.requestLogSink = sinkB
	engineA := newAffinityTestEngine(t, handlerA)
	engineB := newAffinityTestEngine(t, handlerB)

	// A learns the shared prefix on its second credential; B's own scheduler
	// would start from the first, so only the shared mapping explains sk-two.
	serveAffinityRequest(t, engineA, `{"model":"gpt-4o","messages":[{"role":"user","content":"warm-up"}]}`)
	serveAffinityRequest(t, engineA, `{"model":"gpt-4o","messages":[{"role":"user","content":"shared prefix"}]}`)
	serveAffinityRequest(t, engineB, `{"model":"gpt-4o","messages":[{"role":"user","content":"shared prefix"}]}`)
	assertAffinityAttemptKeys(t, forwarderA.inputs, []string{"sk-one", "sk-two"})
	assertAffinityAttemptKeys(t, forwarderB.inputs, []string{"sk-two"})

	// Without Redis the request still succeeds, just without a preference.
	server.Close()
	serveAffinityRequest(t, engineB, `{"model":"gpt-4o","messages":[{"role":"user","content":"shared prefix"}]}`)
	assertAffinityHits(t, sinkB.snapshot(), []bool{true, false})
}

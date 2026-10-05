package gateway

import (
	"net/http"
	"testing"
	"time"

	"gpt-load/internal/cluster"
	"gpt-load/internal/testutil/clustertest"
)

func TestResponsesContinuationCrossesInstancesThroughSharedStore(t *testing.T) {
	_, client := clustertest.NewClient(t)
	instanceA := &scriptedForwarder{results: []UpstreamResult{storedResponse("warm-up"), storedResponse("created-on-a")}}
	instanceB := &scriptedForwarder{results: []UpstreamResult{storedResponse("continued-on-b")}}
	handlerA, engineA, _ := newContinuationFixture(t, instanceA)
	handlerB, engineB, _ := newContinuationFixture(t, instanceB)
	handlerA.responseBindings = cluster.NewResponseBindings(client, time.Hour)
	handlerB.responseBindings = cluster.NewResponseBindings(client, time.Hour)

	// A creates the response on its second credential; B's own scheduler
	// would start from the first, so only shared ownership explains sk-two.
	serveContinuation(t, engineA, "gl-client", `{"model":"gpt-4o","input":"warm-up"}`, http.StatusOK)
	serveContinuation(t, engineA, "gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusOK)
	serveContinuation(t, engineB, "gl-client", `{"model":"gpt-4o","previous_response_id":"created-on-a","input":"continue"}`, http.StatusOK)

	assertAffinityAttemptKeys(t, instanceA.inputs, []string{"sk-one", "sk-two"})
	assertAffinityAttemptKeys(t, instanceB.inputs, []string{"sk-two"})
	if _, found := lookupTestBinding(t, handlerB, 1, "created-on-a"); !found {
		t.Fatal("B cannot resolve ownership recorded by A")
	}
}

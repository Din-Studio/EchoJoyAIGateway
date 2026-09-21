package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/automodel"
	"gpt-load/internal/coordination"
	"gpt-load/internal/dialect"
	"gpt-load/internal/state"
)

// TestExternalRedisResponseBindingContinuesOnAnotherInstance is opt-in so
// ordinary unit tests stay hermetic. It proves the ownership one instance
// established is the ownership another instance continues from.
func TestExternalRedisResponseBindingContinuesOnAnotherInstance(t *testing.T) {
	client := externalGatewayRedis(t)
	rootID := externalResponseID(t, client, 1)

	creator := &scriptedForwarder{results: []UpstreamResult{storedResponse(rootID)}}
	_, creating, _ := newExternalContinuationInstance(t, client, creator)
	serveContinuation(t, creating, "gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusOK)

	// A second process with its own scheduling state. Its next unbound pick is
	// sk-two, so landing on sk-one can only come from the shared ownership.
	continuing := &scriptedForwarder{results: []UpstreamResult{
		storedResponse(rootID + "-other"), storedResponse(rootID + "-continued"),
	}}
	_, engine, _ := newExternalContinuationInstance(t, client, continuing)
	serveContinuation(t, engine, "gl-client", `{"model":"gpt-4o","input":"unbound"}`, http.StatusOK)
	serveContinuation(t, engine, "gl-client",
		fmt.Sprintf(`{"model":"gpt-4o","previous_response_id":%q,"input":"continue"}`, rootID),
		http.StatusOK)

	assertAffinityAttemptKeys(t, continuing.inputs, []string{"sk-one", "sk-one"})
}

// Ownership belongs to the access key that established it, on every instance.
func TestExternalRedisResponseBindingRejectsAnotherAccessKey(t *testing.T) {
	client := externalGatewayRedis(t)
	rootID := externalResponseID(t, client, 1)

	creator := &scriptedForwarder{results: []UpstreamResult{storedResponse(rootID)}}
	_, creating, _ := newExternalContinuationInstance(t, client, creator)
	serveContinuation(t, creating, "gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusOK)

	continuing := &scriptedForwarder{}
	handler, engine, _ := newExternalContinuationInstance(t, client, continuing)
	other := handler.manager.Current().AccessKeysByHash[handler.encryption.Hash("gl-client")]
	other.ID = 2
	handler.manager.Current().AccessKeysByHash[handler.encryption.Hash("gl-other")] = other
	handler.manager.Current().AccessKeysByID[other.ID] = other
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Redis().Del(ctx, coordination.Key("binding", "2", rootID)).Err()
	})

	serveContinuation(t, engine, "gl-other",
		fmt.Sprintf(`{"model":"gpt-4o","previous_response_id":%q,"input":"continue"}`, rootID),
		http.StatusBadRequest)
	if len(continuing.inputs) != 0 {
		t.Fatal("another access key reached an upstream through someone else's ownership")
	}
}

// The auto-model preset frozen with the ownership has to cross instances with
// it, or a continuation would be answered by a different model.
func TestExternalRedisResponseBindingCarriesTheFrozenPreset(t *testing.T) {
	client := externalGatewayRedis(t)
	rootID := externalResponseID(t, client, 1)

	selection := &automodel.Selection{
		EntryID: "auto-probe", EntryName: "auto-probe", PresetID: "old-preset", PresetName: "old preset",
		TargetModel: "gpt-4o", ConfigRevision: 1,
		ParameterOverrides: json.RawMessage(
			`[{"match":{"protocol":"openai-responses"},"set":{"reasoning":{"effort":"high"}}}]`),
	}
	creating, _, _ := newHandlerForTest(t, &scriptedForwarder{}, "key-a", "key-b")
	creating.responseBindings = coordination.NewResponseBindings(client)
	if !recordTestBinding(t, creating, 1, rootID,
		state.CredentialRef{ID: 1, GroupID: 1, IdentityGeneration: 1}, selection) {
		t.Fatal("the creating instance could not establish ownership")
	}

	forwarder := &scriptedForwarder{results: []UpstreamResult{{
		StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: []byte(fmt.Sprintf(`{"id":%q,"object":"response","model":"gpt-4o"}`, rootID+"-next")),
	}}}
	continuing, continuingManager, _ := newHandlerForTest(t, forwarder, "key-a", "key-b")
	continuing.dialects = dialect.NewSet(dialect.NewOpenAI(), dialect.NewOpenAIResponses())
	configureAutoModelTest(t, continuing, continuingManager, state.FilterSet{})
	continuing.responseBindings = coordination.NewResponseBindings(client)
	continuing.decisionClient = autoDecisionClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("a continuation carrying a frozen preset must not reclassify")
		return nil, nil
	})
	sink := &recordingRequestLogSink{}
	continuing.requestLogSink = sink
	engine := gin.New()
	bindGatewayRoutesForTest(t, engine, continuing)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(fmt.Sprintf(
		`{"model":"auto-probe","previous_response_id":%q,"input":[{"type":"function_call_output","call_id":"call-1","output":"result"}]}`,
		rootID)))
	request.Header.Set("Authorization", "Bearer gl-client")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK || len(forwarder.inputs) != 1 ||
		!strings.Contains(string(forwarder.inputs[0].Request.Body), `"effort":"high"`) {
		t.Fatalf("continuation status = %d, body = %s", response.Code, response.Body)
	}
	events := sink.snapshot()
	if len(events) != 1 || events[0].AutoDecision == nil ||
		events[0].AutoDecision.Selection.PresetID != "old-preset" {
		t.Fatalf("continuation observation = %#v", events)
	}
}

// An unreachable coordination backend is the gateway's own failure and must be
// reported as one, in the response and in the request log alike.
func TestExternalRedisResponseBindingFailsClosedWhenRedisIsGone(t *testing.T) {
	client := externalGatewayRedis(t)
	rootID := externalResponseID(t, client, 1)

	creator := &scriptedForwarder{results: []UpstreamResult{storedResponse(rootID)}}
	_, creating, _ := newExternalContinuationInstance(t, client, creator)
	serveContinuation(t, creating, "gl-client", `{"model":"gpt-4o","input":"initial"}`, http.StatusOK)

	severed := externalGatewayRedis(t)
	if err := severed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	continuing := &scriptedForwarder{}
	handler, engine, sink := newContinuationFixture(t, continuing)
	handler.responseBindings = coordination.NewResponseBindings(severed)

	response := serveContinuation(t, engine, "gl-client",
		fmt.Sprintf(`{"model":"gpt-4o","previous_response_id":%q,"input":"continue"}`, rootID),
		http.StatusServiceUnavailable)
	if !strings.Contains(response.Body.String(), reasonCoordinationUnavailable.Code) {
		t.Fatalf("body = %s, want %q", response.Body.String(), reasonCoordinationUnavailable.Code)
	}
	if len(continuing.inputs) != 0 {
		t.Fatal("an unresolved owner reached an upstream")
	}
	events := sink.snapshot()
	if got := events[len(events)-1].ErrorCode; got != reasonCoordinationUnavailable.Code {
		t.Fatalf("request log error code = %q, want %q", got, reasonCoordinationUnavailable.Code)
	}
}

func newExternalContinuationInstance(
	t *testing.T,
	client *coordination.Client,
	forwarder AttemptForwarder,
) (*Handler, *gin.Engine, *recordingRequestLogSink) {
	t.Helper()
	handler, engine, sink := newContinuationFixture(t, forwarder)
	handler.responseBindings = coordination.NewResponseBindings(client)
	return handler, engine, sink
}

func externalGatewayRedis(t *testing.T) *coordination.Client {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GPT_LOAD_REDIS_TEST_DSN"))
	if dsn == "" {
		t.Skip("GPT_LOAD_REDIS_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := coordination.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// externalResponseID namespaces the ids this run writes, so parallel CI jobs
// cannot observe each other's ownership.
func externalResponseID(t *testing.T, client *coordination.Client, accessKeyID uint) string {
	t.Helper()
	responseID := fmt.Sprintf("resp_test-%d-%s", time.Now().UnixNano(), t.Name())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		keys := []string{
			coordination.Key("binding", fmt.Sprint(accessKeyID), responseID),
			coordination.Key("binding", fmt.Sprint(accessKeyID), responseID+"-other"),
			coordination.Key("binding", fmt.Sprint(accessKeyID), responseID+"-continued"),
			coordination.Key("binding", fmt.Sprint(accessKeyID), responseID+"-next"),
		}
		if err := client.Redis().Del(ctx, keys...).Err(); err != nil {
			t.Errorf("delete test binding keys: %v", err)
		}
	})
	return responseID
}

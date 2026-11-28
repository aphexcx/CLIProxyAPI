package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// seatMoveTableRow is one row of the claude-cache table's moves block, read
// through the table's JSON as the management route serves it.
type seatMoveTableRow struct {
	Relation string `json:"relation"`
	Warm     bool   `json:"warm"`
	Kept     int64  `json:"kept"`
	RePaid   int64  `json:"re_paid"`
	Read     int64  `json:"read"`
	Written  int64  `json:"written"`
}

func seatMoveRow(t *testing.T, relation string, warm bool) seatMoveTableRow {
	t.Helper()
	raw, errMarshal := json.Marshal(helps.DefaultClaudeCacheMeter().Snapshot())
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var table struct {
		Days []struct {
			Moves []seatMoveTableRow `json:"moves"`
		} `json:"days"`
	}
	if errUnmarshal := json.Unmarshal(raw, &table); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if len(table.Days) == 0 {
		return seatMoveTableRow{}
	}
	for _, row := range table.Days[0].Moves {
		if row.Relation == relation && row.Warm == warm {
			return row
		}
	}
	return seatMoveTableRow{}
}

// A moved conversation's reply is filed in the claude-cache table under the
// move's relation and warm flag: the seat router tags the move, the executor
// passes the tag to the meter, and the meter files the reply as KEPT or RE-PAID.
// The fixture reply reads 40 and writes 100 with 3 uncached input tokens, so
// it is RE-PAID (40 is less than half of 143).
func TestClaudeSeatMoveFiledInCacheTable(t *testing.T) {
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			cfg := &config.Config{}
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewClaudeExecutor(cfg))
			upstreams := map[string]*oneHourUpstream{}
			for _, suffix := range []string{"a", "b"} {
				server, upstream := newOneHourUpstream(t, nil)
				id := "seat-move-" + t.Name() + "-" + suffix
				upstreams[id] = upstream
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: oneHourModel}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
					ID: id, Provider: "claude", Status: cliproxyauth.StatusActive,
					Attributes: map[string]string{"api_key": oneHourKeySeatKey, "base_url": server.URL},
					Metadata:   map[string]any{"organization_uuid": "00000000-0000-4000-8000-00000000000a"},
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			payload := []byte(`{"model":"` + oneHourModel + `","max_tokens":16,"stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"hello"}]}`)
			call := func() {
				t.Helper()
				opts := cliproxyexecutor.Options{Stream: stream, SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload,
					Headers: http.Header{"X-Session-Id": []string{"seat-move-fixture-session"}}, Metadata: map[string]any{}}
				req := cliproxyexecutor.Request{Model: oneHourModel, Payload: payload}
				if !stream {
					if _, errExecute := manager.Execute(context.Background(), []string{"claude"}, req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
					return
				}
				result, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
				if errStream != nil {
					t.Fatal(errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			}

			call()
			first := ""
			for id, upstream := range upstreams {
				if upstream.count() == 1 {
					first = id
				}
			}
			if first == "" {
				t.Fatal("the first request reached no seat")
			}
			auth, _ := manager.GetByID(first)
			auth.Disabled = true
			if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}

			before := seatMoveRow(t, "same_org", true)
			call()
			after := seatMoveRow(t, "same_org", true)
			for id, upstream := range upstreams {
				if id != first && upstream.count() != 1 {
					t.Fatalf("the moved request did not reach the other seat")
				}
			}
			if got := after.RePaid - before.RePaid; got != 1 {
				t.Fatalf("moves same_org/warm: re_paid += %d, want 1 (table row %+v)", got, after)
			}
			if got := after.Kept - before.Kept; got != 0 {
				t.Fatalf("moves same_org/warm: kept += %d, want 0", got)
			}
			if got := after.Read - before.Read; got != 40 {
				t.Fatalf("moves same_org/warm: read += %d, want 40", got)
			}
			if got := after.Written - before.Written; got != 100 {
				t.Fatalf("moves same_org/warm: written += %d, want 100", got)
			}
		})
	}
}

// The tag is a measure only: the request sent upstream (path, headers and
// body) is byte for byte the same with the move tag as without it.
func TestClaudeSeatMoveTagLeavesUpstreamRequestUnchanged(t *testing.T) {
	const hash = "d1b2c3d4e5f60718293a4b5c6d7e8f90"
	for _, native := range []bool{false, true} {
		for _, stream := range oneHourPaths {
			t.Run(map[bool]string{false: "plain/", true: "native/"}[native]+oneHourPathName(stream), func(t *testing.T) {
				headers := http.Header{"X-Session-Id": []string{"seat-move-fixture-session"}}
				if native {
					headers = oneHourNativeHeaders()
				}
				payload := oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", "")
				untagged, _, errUntagged := runOneHourCall(t, oneHourCall{stream: stream, headers: headers, payload: payload,
					metadata: map[string]any{cliproxyexecutor.ClaudeSeatClientHashMetadataKey: hash}})
				if errUntagged != nil {
					t.Fatalf("untagged request error = %v", errUntagged)
				}
				tagged, _, errTagged := runOneHourCall(t, oneHourCall{stream: stream, headers: headers, payload: payload,
					metadata: map[string]any{cliproxyexecutor.ClaudeSeatClientHashMetadataKey: hash,
						cliproxyexecutor.ClaudeSeatMoveMetadataKey: cliproxyexecutor.ClaudeSeatMove{
							Relation: cliproxyexecutor.ClaudeSeatMoveOtherOrg, Warm: true, Reason: "credential_cooldown"}}})
				if errTagged != nil {
					t.Fatalf("tagged request error = %v", errTagged)
				}
				if untagged.count() != 1 || tagged.count() != 1 {
					t.Fatalf("upstream saw %d and %d requests, want 1 and 1", untagged.count(), tagged.count())
				}
				if !bytes.Equal(tagged.body(0), untagged.body(0)) {
					t.Fatalf("tagged body differs:\ntagged:   %s\nuntagged: %s", tagged.body(0), untagged.body(0))
				}
				if !reflect.DeepEqual(tagged.header(0), untagged.header(0)) {
					t.Fatalf("tagged headers differ:\ntagged:   %v\nuntagged: %v", tagged.header(0), untagged.header(0))
				}
				if tagged.paths[0] != untagged.paths[0] {
					t.Fatalf("tagged path %s, untagged %s", tagged.paths[0], untagged.paths[0])
				}
			})
		}
	}
}

// The one-hour cache rule's refusal re-send: the refused attempt is abandoned
// and the re-sent request files the moved reply exactly once.
func TestClaudeSeatMoveFiledOnceAcrossOneHourRefusalResend(t *testing.T) {
	const hash = "e1b2c3d4e5f60718293a4b5c6d7e8f90"
	for _, stream := range oneHourPaths {
		t.Run(oneHourPathName(stream), func(t *testing.T) {
			before := seatMoveRow(t, "same_seat", false)
			upstream, _, err := runOneHourCall(t, oneHourCall{stream: stream, payload: oneHourPayload(oneHourBare, oneHourBare, oneHourBare, "", ""),
				refuse: oneHourRefuseHour, metadata: map[string]any{cliproxyexecutor.ClaudeSeatClientHashMetadataKey: hash,
					cliproxyexecutor.ClaudeSeatMoveMetadataKey: cliproxyexecutor.ClaudeSeatMove{
						Relation: cliproxyexecutor.ClaudeSeatMoveSameSeat, Warm: false, Reason: "cache_expired"}}})
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			if got := upstream.count(); got != 2 {
				t.Fatalf("upstream saw %d requests, want the refused one and the re-send", got)
			}
			after := seatMoveRow(t, "same_seat", false)
			if got := after.RePaid + after.Kept - before.RePaid - before.Kept; got != 1 {
				t.Fatalf("moves same_seat/expired filed %d replies, want 1 (table row %+v)", got, after)
			}
			if got := after.Read - before.Read; got != 40 {
				t.Fatalf("moves same_seat/expired: read += %d, want 40", got)
			}
		})
	}
}

// The class of selections whose reply is never filed (a token count, a failed
// attempt) must not consume a move: the move stays pending until the first
// metered reply of the conversation files it, and it is filed exactly once.
func TestClaudeSeatMoveSurvivesUnmeteredSelections(t *testing.T) {
	for _, tc := range []struct{ unmetered, generate string }{
		{unmetered: "count", generate: "execute"},
		{unmetered: "count", generate: "stream"},
		{unmetered: "failed", generate: "execute"},
		{unmetered: "failed", generate: "stream"},
	} {
		t.Run(tc.unmetered+" then "+tc.generate, func(t *testing.T) {
			cfg := &config.Config{}
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewClaudeExecutor(cfg))
			var refuseNext atomic.Bool
			refuse := func([]byte) string {
				if refuseNext.CompareAndSwap(true, false) {
					return oneHourOtherRefusal
				}
				return ""
			}
			upstreams := map[string]*oneHourUpstream{}
			organizations := []string{"00000000-0000-4000-8000-00000000000b", "00000000-0000-4000-8000-00000000000c"}
			for i, suffix := range []string{"a", "b"} {
				server, upstream := newOneHourUpstream(t, refuse)
				id := "seat-move-unmetered-" + t.Name() + "-" + suffix
				upstreams[id] = upstream
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: oneHourModel}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
					ID: id, Provider: "claude", Status: cliproxyauth.StatusActive,
					Attributes: map[string]string{"api_key": oneHourKeySeatKey, "base_url": server.URL},
					Metadata:   map[string]any{"organization_uuid": organizations[i]},
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			call := func(kind string) error {
				payload := []byte(`{"model":"` + oneHourModel + `","max_tokens":16,"stream":` + strconv.FormatBool(kind == "stream") + `,"messages":[{"role":"user","content":"hello"}]}`)
				opts := cliproxyexecutor.Options{Stream: kind == "stream", SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload,
					Headers: http.Header{"X-Session-Id": []string{"seat-move-unmetered-session"}}, Metadata: map[string]any{}}
				req := cliproxyexecutor.Request{Model: oneHourModel, Payload: payload}
				switch kind {
				case "count":
					_, errCount := manager.ExecuteCount(context.Background(), []string{"claude"}, req, opts)
					return errCount
				case "stream":
					result, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
					if errStream != nil {
						return errStream
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				default:
					_, errExecute := manager.Execute(context.Background(), []string{"claude"}, req, opts)
					return errExecute
				}
			}

			if errFirst := call(tc.generate); errFirst != nil {
				t.Fatal(errFirst)
			}
			first := ""
			for id, upstream := range upstreams {
				if upstream.count() == 1 {
					first = id
				}
			}
			if first == "" {
				t.Fatal("the first request reached no seat")
			}
			auth, _ := manager.GetByID(first)
			auth.Disabled = true
			if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}

			before := seatMoveRow(t, "other_org", true)
			if tc.unmetered == "count" {
				if errCount := call("count"); errCount != nil {
					t.Fatal(errCount)
				}
			} else {
				refuseNext.Store(true)
				if errFailed := call(tc.generate); errFailed == nil {
					t.Fatal("the refused request succeeded")
				}
			}
			if row := seatMoveRow(t, "other_org", true); row != before {
				t.Fatalf("the %s request filed a move: %+v", tc.unmetered, row)
			}
			if errGenerate := call(tc.generate); errGenerate != nil {
				t.Fatal(errGenerate)
			}
			after := seatMoveRow(t, "other_org", true)
			if got := after.RePaid + after.Kept - before.RePaid - before.Kept; got != 1 {
				t.Fatalf("moves other_org/warm filed %d replies after the %s request, want 1 (table row %+v)", got, tc.unmetered, after)
			}
			if errAgain := call(tc.generate); errAgain != nil {
				t.Fatal(errAgain)
			}
			if again := seatMoveRow(t, "other_org", true); again != after {
				t.Fatalf("the move was filed again by a later reply: %+v", again)
			}
		})
	}
}

// Requests of one conversation can overlap. The move belongs to the request
// placed first after it, whatever order the replies finish in: here the moved
// stream R1 re-writes its context but finishes after R2, a later request whose
// reply reads from cache, and the table files R1's re-paid reply, not R2's.
func TestClaudeSeatMoveFiledByFirstPlacedRequest(t *testing.T) {
	const (
		heldReply = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_held\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"cache_creation_input_tokens\":100,\"cache_read_input_tokens\":0,\"cache_creation\":{\"ephemeral_5m_input_tokens\":100,\"ephemeral_1h_input_tokens\":0}}}}\n\n"
		heldTail  = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		keptReply = `{"id":"msg_kept","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":100}}`
	)
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	var keysMu sync.Mutex
	var keys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		keysMu.Lock()
		key := r.Header.Get("X-Api-Key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		keys = append(keys, key)
		keysMu.Unlock()
		if strings.Contains(string(body), "held-request") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, heldReply)
			w.(http.Flusher).Flush()
			arrived <- struct{}{}
			<-release
			_, _ = io.WriteString(w, heldTail)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, keptReply)
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{}
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(NewClaudeExecutor(cfg))
	ids := []string{"seat-move-overlap-a", "seat-move-overlap-b"}
	for i, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: oneHourModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
			ID: id, Provider: "claude", Status: cliproxyauth.StatusActive,
			Attributes: map[string]string{"api_key": oneHourKeySeatKey + "-" + id, "base_url": upstream.URL},
			Metadata:   map[string]any{"organization_uuid": []string{"00000000-0000-4000-8000-00000000000d", "00000000-0000-4000-8000-00000000000e"}[i]},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	options := func(text string, stream bool) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
		payload := []byte(`{"model":"` + oneHourModel + `","max_tokens":16,"stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"` + text + `"}]}`)
		return cliproxyexecutor.Request{Model: oneHourModel, Payload: payload}, cliproxyexecutor.Options{Stream: stream, SourceFormat: sdktranslator.FormatClaude,
			OriginalRequest: payload, Headers: http.Header{"X-Session-Id": []string{"seat-move-overlap-session"}}, Metadata: map[string]any{}}
	}

	req, opts := options("hello", false)
	if _, errFirst := manager.Execute(context.Background(), []string{"claude"}, req, opts); errFirst != nil {
		t.Fatal(errFirst)
	}
	// Disable the seat that served the first request: the next one is a move.
	keysMu.Lock()
	firstKey := keys[0]
	keysMu.Unlock()
	disabled := false
	for _, id := range ids {
		if firstKey != oneHourKeySeatKey+"-"+id {
			continue
		}
		auth, _ := manager.GetByID(id)
		auth.Disabled = true
		if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
			t.Fatal(errUpdate)
		}
		disabled = true
	}
	if !disabled {
		t.Fatalf("no seat sends the first request's key %q", firstKey)
	}
	before := seatMoveRow(t, "other_org", true)

	held := make(chan error, 1)
	go func() {
		heldReq, heldOpts := options("held-request", true)
		result, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, heldReq, heldOpts)
		if errStream != nil {
			held <- errStream
			return
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				held <- chunk.Err
				return
			}
		}
		held <- nil
	}()
	select {
	case <-arrived:
	case errHeld := <-held:
		t.Fatalf("the held stream ended before it reached the upstream: %v", errHeld)
	case <-time.After(30 * time.Second):
		t.Fatal("the held stream never reached the upstream")
	}
	req, opts = options("hello", false)
	if _, errLater := manager.Execute(context.Background(), []string{"claude"}, req, opts); errLater != nil {
		t.Fatal(errLater)
	}
	close(release)
	select {
	case errHeld := <-held:
		if errHeld != nil {
			t.Fatal(errHeld)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the held stream did not finish")
	}

	after := seatMoveRow(t, "other_org", true)
	got := seatMoveTableRow{Kept: after.Kept - before.Kept, RePaid: after.RePaid - before.RePaid, Read: after.Read - before.Read, Written: after.Written - before.Written}
	if want := (seatMoveTableRow{RePaid: 1, Written: 100}); got != want {
		t.Fatalf("moves other_org/warm changed by %+v, want the first placed request's re-paid reply %+v", got, want)
	}
}

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"

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

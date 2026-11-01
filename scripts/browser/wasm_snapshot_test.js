/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import { assert, assertEquals, assertThrows } from "jsr:@std/assert@1";
import { closeRestoredSockets, deadSocket, instrument, isSocket, socketEntry } from "./wasm_snapshot.js";

// fakeGo is the part of wasm_exec's Go that socket capture and restore touch:
// func wrappers record their calls.
function fakeGo() {
  const calls = [];
  const go = {
    calls,
    _values: [NaN, 0, null, true, false, globalThis, null],
    _makeFuncWrapper(id) {
      return function (event) {
        calls.push({ id, event, self: this });
      };
    },
    _resume() {},
    run() {},
    importObject: { gojs: {} },
  };
  go._values[6] = go;
  instrument(go);
  return go;
}

// Deno's WebSocket only takes handlers on a connected instance; isSocket reads
// globalThis.WebSocket when called, so a plain class stands in for it.
class FakeWebSocket {}
globalThis.WebSocket = FakeWebSocket;

function socketLike() {
  return new FakeWebSocket();
}

Deno.test("socketEntry records the onclose func wrapper Go set", () => {
  const go = fakeGo();
  const ws = socketLike();
  assert(isSocket(ws));
  ws.onclose = go._makeFuncWrapper(42);
  assertEquals(socketEntry(ws), { k: "socket", onclose: 42 });
});

Deno.test("socketEntry without a Go onclose has no handler id", () => {
  const ws = socketLike();
  ws.onclose = () => {};
  assertEquals(socketEntry(ws), { k: "socket" });
});

Deno.test("isSocket is false for plain objects", () => {
  assert(!isSocket({ readyState: 1 }));
  assert(!isSocket(null));
});

Deno.test("deadSocket is closed and refuses sends", () => {
  const s = deadSocket();
  assertEquals(s.readyState, 3);
  s.close();
  assertThrows(() => s.send("x"), Error, "did not survive");
});

Deno.test("closeRestoredSockets fires the captured onclose with an abnormal close", () => {
  const go = fakeGo();
  const socket = deadSocket();
  closeRestoredSockets(go, [{ socket, onclose: 7 }]);
  assertEquals(go.calls.length, 1);
  assertEquals(go.calls[0].id, 7);
  assertEquals(go.calls[0].self, socket);
  assertEquals(go.calls[0].event.code, 1006);
  assertEquals(go.calls[0].event.wasClean, false);
});

Deno.test("closeRestoredSockets reuses a restored wrapper for the same id", () => {
  const go = fakeGo();
  const wrapper = go._makeFuncWrapper(9);
  go._values.push(wrapper);
  const made = [];
  const make = go._makeFuncWrapper;
  go._makeFuncWrapper = (id) => (made.push(id), make(id));
  closeRestoredSockets(go, [{ socket: deadSocket(), onclose: 9 }]);
  assertEquals(made, []);
  assertEquals(go.calls.map((c) => c.id), [9]);
});

Deno.test("closeRestoredSockets prefers an onclose Go set on the stand-in", () => {
  const go = fakeGo();
  const socket = deadSocket();
  socket.onclose = go._makeFuncWrapper(11);
  closeRestoredSockets(go, [{ socket, onclose: 7 }]);
  assertEquals(go.calls.map((c) => c.id), [11]);
});

Deno.test("closeRestoredSockets skips a socket without a handler", () => {
  const go = fakeGo();
  closeRestoredSockets(go, [{ socket: deadSocket() }]);
  assertEquals(go.calls, []);
});

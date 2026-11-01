// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { agentTokenNote } from "./vmshell.js";

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`${label}: expected ${e}, got ${a}`);
}

const agent = (phase, atircToken) => ({ name: "c", phase, agent: true, atircToken });

Deno.test("agentTokenNote: waiting for the receipt, then leased", () => {
  assertEquals(agentTokenNote(agent("Held")), "agent token: waiting for receipt");
  assertEquals(agentTokenNote(agent("Held", { status: "True", reason: "Leased", message: "the atirc agent token is in the bundle (Leased, attempt 1)" })), "agent token: leased");
  assertEquals(agentTokenNote(agent("Held", { status: "True", reason: "Unleased" })), "agent token: minted without a lease (the atirc server has no leases)");
  assertEquals(agentTokenNote(agent("Held", { status: "True", reason: "Present" })), "agent token: in the bundle");
});

Deno.test("agentTokenNote: mint failures carry the condition's message", () => {
  assertEquals(
    agentTokenNote(agent("Held", { status: "False", reason: "MintFailed", message: "attempt 3 of 12: lease refused: receipt not verified" })),
    "agent token: mint failed, retrying: attempt 3 of 12: lease refused: receipt not verified",
  );
  assertEquals(
    agentTokenNote(agent("Held", { status: "False", reason: "GaveUp", message: "no atirc agent token after 12 attempts" })),
    "agent token: mint GAVE UP: no atirc agent token after 12 attempts",
  );
});

Deno.test("agentTokenNote: a mint without a leases claim is refused (r131); Unleased still reads", () => {
  assertEquals(
    agentTokenNote(agent("Held", { status: "False", reason: "LeaseClaimMissing", message: "the minted token has no leases claim" })),
    "agent token: server returned no lease (refused)",
  );
  assertEquals(agentTokenNote(agent("Held", { status: "True", reason: "Unleased" })), "agent token: minted without a lease (the atirc server has no leases)");
});

Deno.test("agentTokenNote: nothing for contracts without a token, or finished before one", () => {
  assertEquals(agentTokenNote({ name: "c", phase: "Held" }), "", "no agent, no R2");
  assertEquals(agentTokenNote({ name: "c", phase: "Held", guestTools: ["socialweb-computer"] }), "agent token: waiting for receipt", "R2 mints too");
  assertEquals(agentTokenNote(agent("Released")), "", "released before any mint");
  assertEquals(agentTokenNote(agent("Released", { status: "True", reason: "Leased" })), "agent token: leased", "history keeps the outcome");
});

Deno.test("page wiring: the row shows the agent token note", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  if (!/vmshell\.agentTokenNote\(contract\),\s*\.\.\.vmshell\.releaseNotes/.test(app)) throw new Error("row lacks agentTokenNote");
});

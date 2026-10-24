// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  contractManifest,
  contractName,
  parseBidders,
  randomToken,
  secretsBundle,
  shellLink,
} from "./vmshell.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

Deno.test("token is 32 base64url chars", () => {
  const token = randomToken();
  assert(/^[A-Za-z0-9_-]{32}$/.test(token), token);
});

Deno.test("contract name is a DNS label", () => {
  assert(contractName(new Uint8Array([0, 1, 35, 36, 255, 10])) === "vmshell-ab9adk", contractName(new Uint8Array([0, 1, 35, 36, 255, 10])));
  assert(/^vmshell-[a-z0-9]{6}$/.test(contractName()), "random name");
});

Deno.test("manifest carries secretsRef and the shell tunnel", () => {
  const m = contractManifest({ name: "vmshell-abc123", bidders: parseBidders(""), ownerDid: "did:plc:me" });
  assert(m.spec.secretsRef.mount === "requester", "mount");
  assert(m.spec.secretsRef.path === "contracts/vmshell-abc123/secrets", "path");
  assert(m.spec.bidderDids[0] === "did:plc:4vcig7s43z3nyqrnepmcfzqt", "default bidder");
  assert(JSON.stringify(m.spec.tunnels) === '[{"tag":"shell","port":7681,"shell":{"envFile":"/etc/wootty/wootty.env"}}]', "tunnels");
  assert(m.spec.ownerDid === "did:plc:me" && m.spec.userData === undefined, "owner, no userData");
  assert(parseBidders("did:plc:a, did:plc:b junk").join() === "did:plc:a,did:plc:b", "bidders");
});

Deno.test("bundle delivers the wootty env file", () => {
  const entries = JSON.parse(secretsBundle("tok"));
  assert(entries.length === 1 && entries[0].path === "/etc/wootty/wootty.env", "path");
  assert(entries[0].value === "WOOTTY_AUTH_TOKEN=tok\n", "value");
});

Deno.test("shell link uses the shell network and the token fragment", () => {
  const contract = {
    networks: [
      { tag: "other", address: "https://x.example" },
      { tag: "shell", address: "https://abcd.ef12.opentunnel.xyz" },
    ],
  };
  assert(shellLink(contract, "a+b/c") === "https://abcd.ef12.opentunnel.xyz/#token=a%2Bb%2Fc", shellLink(contract, "a+b/c"));
  assert(shellLink({ networks: [] }, "t") === "", "no address yet");
  assert(shellLink({ networks: [{ tag: "shell", address: "javascript:alert(1)" }] }, "t") === "", "non-https rejected");
  assert(shellLink(contract, "") === "", "no token");
});
